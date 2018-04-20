package dag

import (
	"context"
	"fmt"
	"strings"

	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
	"gonum.org/v1/gonum/graph/encoding/dot"
	gpath "gonum.org/v1/gonum/graph/path"
	"gonum.org/v1/gonum/graph/simple"
	"gonum.org/v1/gonum/graph/topo"
)

// labeledNode allows the dot graph output to have labeled nodes instead of ID numbers
type labeledNode struct {
	simple.Node
	label string
}

func (ln *labeledNode) DOTID() string {
	return ln.label
}

// GraphObject describes an object that will become a node in the graph
type GraphObject interface {
	Name() string   // the unique name for the object
	String() string // the pretty-printed string for the object. Can be one of the following:
	//  - a string of alphabetic ([a-zA-Z\x80-\xff]) characters, underscores ('_').
	//    digits ([0-9]), not beginning with a digit.
	//  - a numeral [-]?(.[0-9]+ | [0-9]+(.[0-9]*)?).
	//  - a double-quoted string ("...") possibly containing escaped quotes (\").
	//  - an HTML string (<...>)
	Dependencies() []string // names of dependencies, in any order
}

// ObjectGraph builds and analyzes a graph of the supplied objects
type ObjectGraph struct {
	objs    []GraphObject
	g       *simple.DirectedGraph
	root    int64
	idmap   map[int64]string
	namemap map[string]int64
	levels  [][]GraphObject
}

func (og *ObjectGraph) init() {
	og.idmap = make(map[int64]string)
	og.namemap = make(map[string]int64)
	og.levels = [][]GraphObject{}
}

func (og *ObjectGraph) populate(objs []GraphObject) error {
	dg := simple.NewDirectedGraph()
	// add all nodes
	for i, o := range objs {
		offset := int64(i)
		if o.Name() == "" {
			return fmt.Errorf("empty object name at offset %v", i)
		}
		if o.Name() == rootName {
			return fmt.Errorf("reserved name at offset %v: %v", i, rootName)
		}
		og.idmap[offset] = o.Name()
		og.namemap[o.Name()] = offset
		dg.AddNode(&labeledNode{Node: simple.Node(offset), label: o.String()})
	}
	// add all edges
	for i, o := range objs {
		offset := int64(i)
		for _, d := range o.Dependencies() {
			if _, ok := og.namemap[d]; !ok {
				return fmt.Errorf("unknown dependency (of %v): %v", o.Name(), d)
			}
			if offset == og.namemap[d] { // SetEdge panics on this
				return fmt.Errorf("dependency references itself on %v", d)
			}
			dg.SetEdge(dg.NewEdge(dg.Node(offset), dg.Node(og.namemap[d])))
		}
	}
	if cycles := topo.DirectedCyclesIn(dg); len(cycles) > 0 {
		var cstrs []string
		for _, c := range cycles {
			var cstr []string
			for _, n := range c {
				cstr = append(cstr, og.idmap[n.ID()])
			}
			cstrs = append(cstrs, strings.Join(cstr, " -> "))
		}
		return fmt.Errorf("dependency cycles found (%v): %v", len(cycles), strings.Join(cstrs, "; "))
	}
	og.g = dg
	return nil
}

const rootName = "__ROOT__"

type synthRoot struct {
	deps []string
}

func (sr *synthRoot) Name() string {
	return rootName
}
func (sr *synthRoot) String() string {
	return rootName
}
func (sr *synthRoot) Dependencies() []string {
	return sr.deps
}

// setRoot finds the root of the graph or synthetically creates one if there are multiple
func (og *ObjectGraph) setRoot() error {
	roots := []int64{}
	for k := range og.idmap {
		if len(og.g.To(k)) == 0 {
			roots = append(roots, k)
		}
	}
	if len(roots) == 0 { // this should be impossible since we've already checked for cycles
		return errors.New("no graph roots found")
	}
	if len(roots) > 1 {
		nr := &synthRoot{}
		offset := int64(len(og.objs))
		og.idmap[offset] = rootName
		og.namemap[rootName] = offset
		og.g.AddNode(&labeledNode{Node: simple.Node(offset), label: rootName})
		for _, r := range roots {
			nr.deps = append(nr.deps, og.idmap[r])
			og.g.SetEdge(og.g.NewEdge(og.g.Node(offset), og.g.Node(r)))
		}
		og.objs = append(og.objs, nr)
		og.root = offset
	} else {
		og.root = roots[0]
	}
	return nil
}

// calcLevels finds the level (layer) of each node in the DAG by calculating the longest path to each node
func (og *ObjectGraph) calcLevels() {
	wdg := simple.NewWeightedDirectedGraph(0, 0)
	for _, n := range og.g.Nodes() {
		wdg.AddNode(n)
	}
	for _, e := range og.g.Edges() {
		wdg.SetWeightedEdge(wdg.NewWeightedEdge(e.From(), e.To(), -1))
	}
	pt, _ := gpath.BellmanFordFrom(wdg.Node(og.root), wdg) // negative cycles are impossible because this is a DAG
	for _, c := range wdg.Nodes() {
		pth, _ := pt.To(c.ID())
		lvl := len(pth) - 1
		if lvl+1 > len(og.levels) {
			og.levels = append(og.levels, make([][]GraphObject, (lvl+1)-len(og.levels))...)
			og.levels[lvl] = []GraphObject{}
		}
		og.levels[lvl] = append(og.levels[lvl], og.objs[c.ID()])
	}
}

// Build populates the graph with the supplied objects
func (og *ObjectGraph) Build(objs []GraphObject) error {
	og.init()
	og.objs = objs
	if err := og.populate(objs); err != nil {
		return errors.Wrap(err, "error populating graph")
	}
	if err := og.setRoot(); err != nil {
		return errors.Wrap(err, "error getting graph root")
	}
	og.calcLevels()
	return nil
}

// Info returns the root and levels of the graph
func (og *ObjectGraph) Info() (root GraphObject, levels [][]GraphObject, err error) {
	if len(og.objs) == 0 {
		return nil, nil, errors.New("graph is empty")
	}
	return og.objs[og.root], og.levels, nil
}

// Dot returns the GraphWiz DOT output for the graph
func (og *ObjectGraph) Dot(name string) ([]byte, error) {
	b, err := dot.Marshal(og.g, name, "", "    ", true)
	if err != nil {
		return nil, errors.Wrap(err, "error marshaling graph to dot")
	}
	return b, nil
}

// ActionFunc is a function that is executed for each node in the graph. Returning an error will cause the graph walk to abort.
type ActionFunc func(GraphObject) error

// Walk traverses the graph levels in decending order, executing af for every node in a given level concurrently
func (og *ObjectGraph) Walk(ctx context.Context, af ActionFunc) error {
	var g errgroup.Group
	for i := len(og.levels) - 1; i >= 0; i-- {
		for j := range og.levels[i] {
			select {
			case <-ctx.Done():
				return errors.New("context was cancelled")
			default:
			}
			obj := og.levels[i][j]
			if obj.Name() == rootName {
				continue
			}
			g.Go(func() error { return af(obj) })
		}
		if err := g.Wait(); err != nil {
			return errors.Wrapf(err, "error executing level %v", i)
		}
	}
	return nil
}
