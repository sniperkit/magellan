package qtree

import (
	"fmt"
	"sync"

	"github.com/graphql-go/graphql/language/ast"
	"github.com/rgraphql/magellan/types"
	proto "github.com/rgraphql/rgraphql/pkg/proto"
)

type QueryTreeNode struct {
	Id        uint32
	idCounter uint32

	Root     *QueryTreeNode
	Parent   *QueryTreeNode
	Children []*QueryTreeNode

	RootNodeMap    map[uint32]*QueryTreeNode
	SchemaResolver SchemaResolver
	VariableStore  *VariableStore

	FieldName     string
	AST           ast.TypeDefinition
	IsPrimitive   bool
	PrimitiveName string
	Arguments     map[string]*VariableReference

	subCtr         uint32
	subscribers    map[uint32]*qtNodeSubscription
	subscribersMtx sync.Mutex
}

func NewQueryTree(rootQuery *ast.ObjectDefinition, schemaResolver SchemaResolver) *QueryTreeNode {
	nqt := &QueryTreeNode{
		Id:             0,
		RootNodeMap:    map[uint32]*QueryTreeNode{},
		AST:            rootQuery,
		SchemaResolver: schemaResolver,
		VariableStore:  NewVariableStore(),
		subscribers:    make(map[uint32]*qtNodeSubscription),
	}
	nqt.Root = nqt
	nqt.RootNodeMap[0] = nqt
	return nqt
}

// Apply a tree mutation to the tree. Errors leave nodes in a failed state.
func (qt *QueryTreeNode) ApplyTreeMutation(mutation *proto.RGQLTreeMutation) {
	// Apply all variables.
	for _, variable := range mutation.Variables {
		qt.VariableStore.Put(variable)
	}

	for _, aqn := range mutation.NodeMutation {
		// Find the node we are operating on.
		nod, ok := qt.Root.RootNodeMap[aqn.NodeId]
		if !ok {
			continue
		}

		switch aqn.Operation {
		case proto.RGQLTreeMutation_SUBTREE_ADD_CHILD:
			if err := nod.AddChild(aqn.Node); err != nil {
				// TODO: Handle error adding child here.
				// NOTE: we plan to keep the child, but mark it as errored on the client.
				fmt.Printf("Error adding child: %v\n", err)
			}
		case proto.RGQLTreeMutation_SUBTREE_DELETE:
			if aqn.NodeId != 0 && nod != qt.Root {
				nod.Dispose()
			}
		}
	}

	// Garbage collect variables
	qt.VariableStore.GarbageCollect()
}

// AddChild validates and adds a child tree.
func (qt *QueryTreeNode) AddChild(data *proto.RGQLQueryTreeNode) (addChildErr error) {
	// TODO: Defer func, add node even if we get an error.
	// If we have an error: return an error to the client, but keep the node.
	// Allow the node to get deleted later by the client.
	// This keeps a marker in place so that we don't repeatedly evaluate an errant query branch.

	if _, ok := qt.RootNodeMap[data.Id]; ok {
		return fmt.Errorf("Invalid node ID (already exists): %d", data.Id)
	}

	// Figure out the AST for this child.
	od, ok := qt.AST.(*ast.ObjectDefinition)
	if !ok {
		return fmt.Errorf("Invalid node %d, parent is not selectable (%#v).", data.Id, qt.AST)
	}

	var selectedField *ast.FieldDefinition
	for _, field := range od.Fields {
		name := field.Name.Value
		if name == data.FieldName {
			selectedField = field
			break
		}
	}

	if selectedField == nil {
		return fmt.Errorf("Invalid field %s on %s.", data.FieldName, od.Name.Value)
	}

	selectedType := selectedField.Type
	if stl, ok := selectedType.(*ast.List); ok {
		selectedType = stl.Type
	}

	isPrimitive := false
	var primitiveName string
	var selectedTypeDef ast.TypeDefinition
	var namedType *ast.Named

	if n, ok := selectedType.(*ast.NonNull); ok {
		selectedType = n.Type
	}

	if n, ok := selectedType.(*ast.Named); ok {
		namedType = n
		if types.IsPrimitive(n.Name.Value) {
			primitiveName = n.Name.Value
			isPrimitive = true
		}
	}

	if selectedTypeDef == nil && !isPrimitive {
		selectedTypeDef = qt.SchemaResolver.LookupType(selectedType)
		if selectedTypeDef == nil {
			if namedType != nil {
				return fmt.Errorf("Unable to resolve named %s.", namedType.Name.Value)
			}
			return fmt.Errorf("Unable to resolve type %#v.", selectedType)
		}
	}

	argMap := make(map[string]*VariableReference)
	for _, arg := range data.Args {
		vref := qt.VariableStore.Get(arg.VariableId)
		if vref == nil {
			// Cleanup a bit
			for _, marg := range argMap {
				marg.Unsubscribe()
			}
			return fmt.Errorf("Variable id %d not found for argument %s.", arg.VariableId, arg.Name)
		}
		argMap[arg.Name] = vref
	}

	// Mint the new node.
	nnod := &QueryTreeNode{
		Id:             data.Id,
		Parent:         qt,
		Root:           qt.Root,
		SchemaResolver: qt.SchemaResolver,
		VariableStore:  qt.VariableStore,
		FieldName:      data.FieldName,
		AST:            selectedTypeDef,
		IsPrimitive:    isPrimitive,
		PrimitiveName:  primitiveName,
		Arguments:      argMap,
		subscribers:    make(map[uint32]*qtNodeSubscription),
	}
	qt.Children = append(qt.Children, nnod)
	// TODO: Mutex
	qt.Root.RootNodeMap[nnod.Id] = nnod

	// Early failout cleanup defer.
	defer func() {
		if addChildErr != nil {
			qt.removeChild(nnod)
			delete(qt.Root.RootNodeMap, nnod.Id)
		}
	}()

	// Apply any children
	for _, child := range data.Children {
		if err := nnod.AddChild(child); err != nil {
			return err
		}
	}

	// Apply to the resolver tree (start resolution for this node).
	qt.nextUpdate(&QTNodeUpdate{
		Operation: Operation_AddChild,
		Child:     nnod,
	})
	return nil
}

// removeChild deletes the given child from the children array.
func (qt *QueryTreeNode) removeChild(nod *QueryTreeNode) {
	for i, item := range qt.Children {
		if item == nod {
			a := qt.Children
			copy(a[i:], a[i+1:])
			a[len(a)-1] = nil
			qt.Children = a[:len(a)-1]
			qt.nextUpdate(&QTNodeUpdate{
				Operation: Operation_DelChild,
				Child:     item,
			})
			break
		}
	}
}

func (qt *QueryTreeNode) removeSubscription(id uint32) {
	qt.subscribersMtx.Lock()
	delete(qt.subscribers, id)
	qt.subscribersMtx.Unlock()
}

func (qt *QueryTreeNode) SubscribeChanges() QTNodeSubscription {
	qt.subscribersMtx.Lock()
	defer qt.subscribersMtx.Unlock()

	nsub := &qtNodeSubscription{
		id:   qt.subCtr,
		node: qt,
	}
	qt.subCtr++
	qt.subscribers[nsub.id] = nsub
	return nsub
}

func (qt *QueryTreeNode) nextUpdate(update *QTNodeUpdate) {
	qt.subscribersMtx.Lock()
	defer qt.subscribersMtx.Unlock()

	for _, sub := range qt.subscribers {
		sub.nextChange(update)
	}
}

// Dispose deletes the node and all children.
func (qt *QueryTreeNode) Dispose() {
	qt.nextUpdate(&QTNodeUpdate{
		Operation: Operation_Delete,
	})
	for _, child := range qt.Children {
		child.Dispose()
	}
	qt.Children = nil
	if qt.Root != nil && qt.Root.RootNodeMap != nil {
		delete(qt.Root.RootNodeMap, qt.Id)
	}
	if qt.Parent != nil {
		qt.Parent.removeChild(qt)
	}
	if qt.Arguments != nil {
		for _, arg := range qt.Arguments {
			arg.Unsubscribe()
		}
		qt.Arguments = nil
	}
}