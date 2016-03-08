package dispatcher

import (
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"

	log "github.com/Sirupsen/logrus"
	"github.com/docker/swarm-v2/api"
	"github.com/docker/swarm-v2/identity"
	"github.com/docker/swarm-v2/pkg/heartbeat"
	"github.com/docker/swarm-v2/state"
	"golang.org/x/net/context"
)

const (
	defaultHeartBeatPeriod       = 5 * time.Second
	defaultHeartBeatEpsilon      = 500 * time.Millisecond
	defaultGracePeriodMultiplier = 3
)

type registeredNode struct {
	SessionID string
	Heartbeat *heartbeat.Heartbeat
	Tasks     []string
	Node      *api.Node

	mu sync.Mutex
}

// checkSessionID determines if the SessionID has changed and returns the
// appropriate GRPC error code.
//
// This may not belong here in the future.
func (rn *registeredNode) checkSessionID(sessionID string) error {
	rn.mu.Lock()
	defer rn.mu.Unlock()

	// Before each message send, we need to check the nodes sessionID hasn't
	// changed. If it has, we will the stream and make the node
	// re-register.
	if rn.SessionID != sessionID {
		return grpc.Errorf(codes.InvalidArgument, ErrSessionInvalid.Error())
	}

	return nil
}

var (
	// ErrNodeAlreadyRegistered returned if node with same ID was already
	// registered with this dispatcher.
	ErrNodeAlreadyRegistered = errors.New("node already registered")
	// ErrNodeNotRegistered returned if node with such ID wasn't registered
	// with this dispatcher.
	ErrNodeNotRegistered = errors.New("node not registered")
	// ErrSessionInvalid returned when the session in use is no longer valid.
	// The node should re-register and start a new session.
	ErrSessionInvalid = errors.New("session invalid")
)

type periodChooser struct {
	period  time.Duration
	epsilon time.Duration
	rand    *rand.Rand
}

func newPeriodChooser(period, eps time.Duration) *periodChooser {
	return &periodChooser{
		period:  period,
		epsilon: eps,
		rand:    rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (pc *periodChooser) Choose() time.Duration {
	var adj int64
	if pc.epsilon > 0 {
		adj = rand.Int63n(int64(2*pc.epsilon)) - int64(pc.epsilon)
	}
	return pc.period + time.Duration(adj)
}

// Config is configuration for Dispatcher. For default you should use
// DefautConfig.
type Config struct {
	// Addr configures the address the dispatcher reports to agents.
	Addr                  string
	HeartbeatPeriod       time.Duration
	HeartbeatEpsilon      time.Duration
	GracePeriodMultiplier int
}

// DefaultConfig returns default config for Dispatcher.
func DefaultConfig() *Config {
	return &Config{
		HeartbeatPeriod:       defaultHeartBeatPeriod,
		HeartbeatEpsilon:      defaultHeartBeatEpsilon,
		GracePeriodMultiplier: defaultGracePeriodMultiplier,
	}
}

// Dispatcher is responsible for dispatching tasks and tracking agent health.
type Dispatcher struct {
	mu                    sync.Mutex
	addr                  string
	nodes                 map[string]*registeredNode
	store                 state.WatchableStore
	gracePeriodMultiplier int
	periodChooser         *periodChooser
}

// New returns Dispatcher with store.
func New(store state.WatchableStore, c *Config) *Dispatcher {
	return &Dispatcher{
		addr:                  c.Addr,
		nodes:                 make(map[string]*registeredNode),
		store:                 store,
		periodChooser:         newPeriodChooser(c.HeartbeatPeriod, c.HeartbeatEpsilon),
		gracePeriodMultiplier: c.GracePeriodMultiplier,
	}
}

// Register is used for registration of node with particular dispatcher.
func (d *Dispatcher) Register(ctx context.Context, r *api.RegisterRequest) (*api.RegisterResponse, error) {
	log.WithField("request", r).Debugf("(*Dispatcher).Register")
	d.mu.Lock()
	rn, ok := d.nodes[r.Spec.ID]
	d.mu.Unlock()

	if !ok {
		rn = d.registerNode(r.Spec)
	}

	rn.mu.Lock() // take the lock on the node.
	defer rn.mu.Unlock()

	rn.Node.Status.State = api.NodeStatus_READY
	// create or update node in raft
	if err := d.store.Update(func(tx state.Tx) error {
		err := tx.Nodes().Create(rn.Node)
		if err != nil {
			if err != state.ErrExist {
				return err
			}
			if err := tx.Nodes().Update(rn.Node); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}

	// NOTE(stevvooe): We need be a little careful with re-registration. The
	// current implementation just matches the node id and then gives away the
	// sessionID. If we ever want to use sessionID as a secret, which we may
	// want to, this is giving away the keys to the kitchen.
	//
	// The right behavior is going to be informed by identity. Basically, each
	// time a node registers, we invalidate the session and issue a new
	// session, once identity is proven. This will cause misbehaved agents to
	// be kicked when multiple connections are made.
	return &api.RegisterResponse{NodeID: rn.Node.Spec.ID, SessionID: rn.SessionID}, nil
}

func (d *Dispatcher) registerNode(spec *api.NodeSpec) *registeredNode {
	d.mu.Lock()
	defer d.mu.Unlock()

	var (
		// TODO(stevvooe): Validate node specification.
		n = &api.Node{
			Spec: spec,
		}

		nid = n.Spec.ID // prevent the closure from holding onto the entire Spec.
		rn  = &registeredNode{
			SessionID: identity.NewID(), // session ID is local to the dispatcher.
			Heartbeat: heartbeat.New(d.periodChooser.Choose()*time.Duration(d.gracePeriodMultiplier), func() {
				if err := d.nodeDown(nid); err != nil {
					log.Errorf("error deregistering node %s after heartbeat was not received: %v", nid, err)
				}
			}),
			Node: n,
		}
	)

	d.nodes[n.Spec.ID] = rn
	return rn
}

// UpdateTaskStatus updates status of task. Node should send such updates
// on every status change of its tasks.
func (d *Dispatcher) UpdateTaskStatus(ctx context.Context, r *api.UpdateTaskStatusRequest) (*api.UpdateTaskStatusResponse, error) {
	log.WithField("request", r).Debugf("(*Dispatcher).UpdateTaskStatus")
	d.mu.Lock()
	rn, ok := d.nodes[r.NodeID]
	d.mu.Unlock()
	if !ok {
		return nil, grpc.Errorf(codes.NotFound, ErrNodeNotRegistered.Error())
	}

	if err := rn.checkSessionID(r.SessionID); err != nil {
		return nil, err
	}

	err := d.store.Update(func(tx state.Tx) error {
		for _, t := range r.Tasks {
			if err := tx.Tasks().Update(&api.Task{ID: t.ID, Status: t.Status}); err != nil {
				return err
			}
		}
		return nil
	})

	if err != nil {
		return nil, err
	}
	return nil, nil
}

// Tasks is a stream of tasks state for node. Each message contains full list
// of tasks which should be run on node, if task is not present in that list,
// it should be terminated.
func (d *Dispatcher) Tasks(r *api.TasksRequest, stream api.Dispatcher_TasksServer) error {
	log.WithField("request", r).Debugf("(*Dispatcher).Tasks")
	d.mu.Lock()
	rn, ok := d.nodes[r.NodeID]
	d.mu.Unlock()
	if !ok {
		return grpc.Errorf(codes.NotFound, ErrNodeNotRegistered.Error())
	}

	if err := rn.checkSessionID(r.SessionID); err != nil {
		return err
	}

	watchQueue := d.store.WatchQueue()
	nodeTasks := state.Watch(watchQueue,
		state.EventCreateTask{Task: &api.Task{NodeID: r.NodeID},
			Checks: []state.TaskCheckFunc{state.TaskCheckNodeID}},
		state.EventUpdateTask{Task: &api.Task{NodeID: r.NodeID},
			Checks: []state.TaskCheckFunc{state.TaskCheckNodeID}},
		state.EventDeleteTask{Task: &api.Task{NodeID: r.NodeID},
			Checks: []state.TaskCheckFunc{state.TaskCheckNodeID}})
	defer watchQueue.StopWatch(nodeTasks)

	tasksMap := make(map[string]*api.Task)
	err := d.store.View(func(readTx state.ReadTx) error {
		tasks, err := readTx.Tasks().Find(state.ByNodeID(r.NodeID))
		if err != nil {
			return nil
		}
		for _, t := range tasks {
			tasksMap[t.ID] = t
		}
		return nil
	})
	if err != nil {
		return err
	}

	for {
		if err := rn.checkSessionID(r.SessionID); err != nil {
			return err
		}

		var tasks []*api.Task
		for _, t := range tasksMap {
			tasks = append(tasks, t)
		}

		if err := stream.Send(&api.TasksMessage{Tasks: tasks}); err != nil {
			return err
		}

		select {
		case event := <-nodeTasks:
			switch v := event.Payload.(type) {
			case state.EventCreateTask:
				tasksMap[v.Task.ID] = v.Task
			case state.EventUpdateTask:
				tasksMap[v.Task.ID] = v.Task
			case state.EventDeleteTask:
				delete(tasksMap, v.Task.ID)
			}
		case <-stream.Context().Done():
			return nil
		}
	}
}

func (d *Dispatcher) nodeDown(id string) error {
	d.mu.Lock()
	delete(d.nodes, id)
	d.mu.Unlock()

	err := d.store.Update(func(tx state.Tx) error {
		return tx.Nodes().Update(&api.Node{
			Spec:   &api.NodeSpec{ID: id},
			Status: api.NodeStatus{State: api.NodeStatus_DOWN},
		})
	})
	if err != nil {
		return fmt.Errorf("failed to update node %s status to down", id)
	}
	return nil
}

// Heartbeat is heartbeat method for nodes. It returns new TTL in response.
// Node should send new heartbeat earlier than now + TTL, otherwise it will
// be deregistered from dispatcher and its status will be updated to NodeStatus_DOWN
func (d *Dispatcher) Heartbeat(ctx context.Context, r *api.HeartbeatRequest) (*api.HeartbeatResponse, error) {
	log.WithField("request", r).Debugf("(*Dispatcher).Heartbeat")
	d.mu.Lock()
	node, ok := d.nodes[r.NodeID]
	if !ok {
		d.mu.Unlock()
		return nil, grpc.Errorf(codes.NotFound, ErrNodeNotRegistered.Error())
	}

	period := d.periodChooser.Choose() // base period for node
	grace := period * time.Duration(d.gracePeriodMultiplier)

	node.mu.Lock()
	defer node.mu.Unlock()
	d.mu.Unlock()

	if node.SessionID != r.SessionID {
		// We have a hearbeat from an old session, return an error and force
		// the agent to re-register.
		return nil, grpc.Errorf(codes.InvalidArgument, ErrSessionInvalid.Error())
	}

	node.Heartbeat.Update(grace)
	node.Heartbeat.Beat()
	return &api.HeartbeatResponse{Period: period}, nil
}

func (d *Dispatcher) getManagers() []*api.WeightedPeer {
	return []*api.WeightedPeer{
		{
			Addr:   d.addr, // TODO: change after raft
			Weight: 1,
		},
	}
}

// Session is stream which controls agent connection.
// Each message contains list of backup Managers with weights. Also there is
// special boolean field Disconnect which if true indicates that node should
// reconnect to another Manager immediately.
func (d *Dispatcher) Session(r *api.SessionRequest, stream api.Dispatcher_SessionServer) error {
	log.WithField("request", r).Debugf("(*Dispatcher).Session")
	d.mu.Lock()
	rn, ok := d.nodes[r.NodeID]
	d.mu.Unlock()
	if !ok {
		return grpc.Errorf(codes.NotFound, ErrNodeNotRegistered.Error())
	}

	for {
		// After each message send, we need to check the nodes sessionID hasn't
		// changed. If it has, we will the stream and make the node
		// re-register.
		rn.mu.Lock()
		if rn.SessionID != r.SessionID {
			rn.mu.Unlock()
			return grpc.Errorf(codes.InvalidArgument, ErrSessionInvalid.Error())
		}
		rn.mu.Unlock()

		if err := stream.Send(&api.SessionMessage{
			Managers:   d.getManagers(),
			Disconnect: false,
		}); err != nil {
			return err
		}

		time.Sleep(5 * time.Second) // TODO(stevvooe): This should really be watch activated.
	}
}