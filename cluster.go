package cluster

import (
	"math"

	"github.com/electrious-go/kdbush"
)

const (
	// InfinityZoomLevel indicate impossible large Zoom level (Cluster's max is 21).
	InfinityZoomLevel = 100
)

// Cluster struct get a list or stream of geo objects
// and produce all levels of clusters
// Zoom range is limited by 0 to 21, and MinZoom could not be larger, then MaxZoom.
type Cluster struct {
	// MinZoom minimum  Zoom level to generate clusters
	MinZoom int
	// MaxZoom maximum Zoom level to generate clusters
	MaxZoom int
	// PointSize pixel size of marker, affects clustering radius
	PointSize int
	// TileSize size of tile in pixels, affects clustering radius
	TileSize int
	// NodeSize is size of the KD-tree node, 64 by default. Higher means faster indexing but slower search, and vise versa.
	NodeSize int
	// Indexes keeps all KDBush trees
	Indexes []*kdbush.KDBush
	// Points keeps original slice of given points
	Points         []GeoPoint
	clusterIdxSeed int
}

// New create new Cluster instance with default params.
// Will use points and create multilevel clustered indexes.
// All points should implement GeoPoint interface.
// They are not copied in favor of memory efficiency.
// GetCoordinates called only once for each object. Can be recalculated on the fly, if needed.
func New(points []GeoPoint, opts ...Option) (*Cluster, error) {
	cluster := &Cluster{
		MinZoom:   0,
		MaxZoom:   21,
		PointSize: 40, // 240
		TileSize:  512,
		NodeSize:  64,
	}

	for _, opt := range opts {
		err := opt(cluster)
		if err != nil {
			return nil, err
		}
	}
	// limit max Zoom
	if cluster.MaxZoom > 21 {
		cluster.MaxZoom = 21
	}
	// cluster.MaxZoom--
	// adding extra layer for infinite Zoom (initial) layers data storage
	cluster.Indexes = make([]*kdbush.KDBush, cluster.MaxZoom-cluster.MinZoom+2)
	cluster.Points = points
	// get digits number, start from next exponent
	// if we have 78, all cluster will start from 100...
	// if we have 986 points, all clusters ids will start from 1000
	cluster.clusterIdxSeed = int(math.Pow(10, float64(digitsCount(len(points)))))
	clusters := translateGeoPointsToPoints(points)

	for z := cluster.MaxZoom; z >= cluster.MinZoom; z-- {
		// create index from clusters from previous iteration
		cluster.Indexes[z+1-cluster.MinZoom] = kdbush.NewBush(clustersToPoints(clusters), cluster.NodeSize)
		// create clusters for level up using just created index
		clusters = cluster.clusterize(clusters, z)
	}
	// index topmost points
	cluster.Indexes[0] = kdbush.NewBush(clustersToPoints(clusters), cluster.NodeSize)

	return cluster, nil
}

// GetClusters returns the array of clusters for Zoom level.
// The northWest and southEast points are boundary points of square, that should be returned.
// northWest is left topmost point.
// southEast is right bottom point.
// return the object for clustered points,
// X coordinate of returned object is Longitude and
// Y coordinate of returned object is Latitude.
func (c *Cluster) GetClusters(northWest, southEast GeoPoint, zoom int, limit int) []Point {
	zoom = c.LimitZoom(zoom) - c.MinZoom
	index := c.Indexes[zoom]
	nwX, nwY := MercatorProjection(northWest.GetCoordinates())
	seX, seY := MercatorProjection(southEast.GetCoordinates())
	ids := index.Range(nwX, nwY, seX, seY)

	if (limit > 0) && (len(ids) > limit) {
		ids = ids[:limit]
	}

	result := make([]Point, len(ids))

	for i := range ids {
		p := index.Points[ids[i]].(*Point)
		cp := *p
		coordinates := ReverseMercatorProjection(cp.X, cp.Y)
		cp.X = coordinates.Lng
		cp.Y = coordinates.Lat
		result[i] = cp
	}

	return result
}

// GetClustersPointsInRadius will return child points for specific cluster
// this is done with kdbush.Within method allowing fast search.
func (c *Cluster) GetClustersPointsInRadius(clusterID int) []*Point {
	// if clusterID is smaller than initial seed
	// it means that it is original point from which
	// cluster(s) are made
	if clusterID < c.clusterIdxSeed {
		return nil
	}

	originIndex := (clusterID >> 5) - c.clusterIdxSeed
	originZoom := (clusterID % 32) - 1
	originTree := c.Indexes[originZoom]
	originPoint := originTree.Points[originIndex]
	r := float64(c.PointSize) / float64(c.TileSize*(1<<uint(originZoom)))
	treeBelow := c.Indexes[originZoom+1-c.MinZoom]
	ids := treeBelow.Within(originPoint, r)

	var children []*Point

	for _, i := range ids {
		children = append(children, treeBelow.Points[i].(*Point))
	}

	return children
}

// GetClusterExpansionZoom will return how much you need to Zoom to get to a next cluster.
func (c *Cluster) GetClusterExpansionZoom(clusterID int) int {
	if clusterID < c.clusterIdxSeed {
		return c.MaxZoom
	}

	clusterZoom := (clusterID % 32) - 1
	id := clusterID

	for clusterZoom < c.MaxZoom {
		children := c.GetClustersPointsInRadius(id)
		// nil means it is point not cluster
		if children == nil {
			return c.MaxZoom
		}

		clusterZoom++

		if clusterZoom >= c.MaxZoom+1 {
			return c.MaxZoom
		}
		// in case it's more than 1, then return current Zoom
		if len(children) != 1 {
			break
		}

		id = children[0].ID
	}

	return clusterZoom
}

// AllClusters returns all cluster points, array of Point, for Zoom on the map.
// X coordinate of returned object is Longitude and Y coordinate is Latitude.
func (c *Cluster) AllClusters(zoom int, limit int) []Point {
	index := c.Indexes[c.LimitZoom(zoom)-c.MinZoom]
	points := index.Points

	if (limit > 0) && (len(points) > limit) {
		points = points[:limit]
	}

	result := make([]Point, len(points))

	for i := range points {
		p := index.Points[i].(*Point)
		cp := *p
		coordinates := ReverseMercatorProjection(cp.X, cp.Y)
		cp.X = coordinates.Lng
		cp.Y = coordinates.Lat
		result[i] = cp
	}

	return result
}

// clusterize points for Zoom level.
func (c *Cluster) clusterize(points []*Point, zoom int) []*Point {
	var result []*Point

	r := float64(c.PointSize) / float64(c.TileSize*(1<<uint(zoom)))
	index := 0
	// iterate all clusters
	for pi := range points {
		// skip points we have already clustered
		p := points[pi]
		if p.Zoom <= zoom {
			continue
		}
		// mark this point as visited
		p.Zoom = zoom
		// find all neighbours
		tree := c.Indexes[zoom+1-c.MinZoom]
		neighbourIds := tree.Within(&kdbush.SimplePoint{X: p.X, Y: p.Y}, r)
		nPoints := p.NumPoints
		wx := p.X * float64(nPoints)
		wy := p.Y * float64(nPoints)

		var foundNeighbours []*Point

		for j := range neighbourIds {
			b := points[neighbourIds[j]]
			// filter out neighbours, that are processed already (and processed point "p" as well)
			if zoom < b.Zoom {
				wx += b.X * float64(b.NumPoints)
				wy += b.Y * float64(b.NumPoints)
				nPoints += b.NumPoints
				b.Zoom = zoom // set the Zoom to skip in other iterations
				foundNeighbours = append(foundNeighbours, b)
			}
		}
		newCluster := p
		// create new cluster
		if len(foundNeighbours) > 0 {
			newCluster = &Point{}
			newCluster.X = wx / float64(nPoints)
			newCluster.Y = wy / float64(nPoints)
			newCluster.NumPoints = nPoints
			newCluster.Zoom = InfinityZoomLevel
			// create ID based on seed + index
			// this is then shifted to create space for Zoom
			// this is useful when you need extract Zoom from ID
			newCluster.ID = ((c.clusterIdxSeed + index) << 5) + zoom + 1

			for _, neighbour := range foundNeighbours {
				newCluster.Included = append(newCluster.Included, neighbour.Included...)
			}
		}

		result = append(result, newCluster)
		index++
	}

	return result
}

func (c *Cluster) LimitZoom(zoom int) int {
	if zoom > c.MaxZoom {
		zoom = c.MaxZoom
	}

	if zoom < c.MinZoom {
		zoom = c.MinZoom
	}

	return zoom
}
