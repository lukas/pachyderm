package work2

import (
	"context"
	"fmt"
	"path"
	"sync"

	etcd "github.com/coreos/etcd/clientv3"
	"github.com/gogo/protobuf/proto"
	types "github.com/gogo/protobuf/types"
	col "github.com/pachyderm/pachyderm/src/server/pkg/collection"
	"github.com/pachyderm/pachyderm/src/server/pkg/uuid"
	"github.com/pachyderm/pachyderm/src/server/pkg/watch"
	"golang.org/x/sync/errgroup"
)

const (
	subtaskPrefix = "/subtask"
	claimPrefix   = "/claim"
)

// Task thing
type Task interface {
	Run(
		ctx context.Context,
		subtaskData chan *types.Any,
		collect func(ctx context.Context, data *types.Any) error,
	) error
}

// Master thing
type Master interface {
	Clear(ctx context.Context) error
	WithTask(cb func(task Task) error) error
}

// Worker thing
type Worker interface {
	Run(
		ctx context.Context,
		cb func(ctx context.Context, data *types.Any) error,
	) error
}

type taskEtcd struct {
	etcdClient *etcd.Client
	subtasks   col.Collection
	claims     col.Collection
}

type master struct {
	*taskEtcd
}

type worker struct {
	*taskEtcd
}

type task struct {
	master    *master
	id        string
	timestamp *types.Timestamp
}

func newCollection(etcdClient *etcd.Client, etcdPrefix string, template proto.Message) col.Collection {
	return col.NewCollection(
		etcdClient,
		etcdPrefix,
		nil,
		template,
		nil,
		nil,
	)
}

func newTaskEtcd(etcdClient *etcd.Client, etcdPrefix string, taskNamespace string) *taskEtcd {
	return &taskEtcd{
		etcdClient: etcdClient,
		subtasks:   newCollection(etcdClient, path.Join(etcdPrefix, subtaskPrefix, taskNamespace), &Subtask{}),
		claims:     newCollection(etcdClient, path.Join(etcdPrefix, claimPrefix, taskNamespace), &Claim{}),
	}
}

// NewMaster thing
func NewMaster(etcdClient *etcd.Client, etcdPrefix string, taskNamespace string) Master {
	return &master{
		taskEtcd: newTaskEtcd(etcdClient, etcdPrefix, taskNamespace),
	}
}

// NewWorker thing
func NewWorker(etcdClient *etcd.Client, etcdPrefix string, taskNamespace string) Worker {
	return &worker{
		taskEtcd: newTaskEtcd(etcdClient, etcdPrefix, taskNamespace),
	}
}

func (m *master) Clear(ctx context.Context) error {
	_, err := col.NewSTM(ctx, m.etcdClient, func(stm col.STM) error {
		m.subtasks.ReadWrite(stm).DeleteAll()
		return nil
	})
	return err
}

func (m *master) WithTask(cb func(task Task) error) error {
	return cb(&task{timestamp: types.TimestampNow()})
}

type subtaskEntry struct {
	id        string
	timestamp *types.Timestamp
	ctx       context.Context
	cancel    context.CancelFunc
}

func (w *worker) Run(
	ctx context.Context,
	cb func(ctx context.Context, subtask *types.Any) error,
) error {
	eg, ctx := errgroup.WithContext(ctx)

	mutex := sync.Mutex{}
	cond := sync.NewCond(&mutex)

	// Watch subtasks collection for subtasks in the RUNNING state, organize by task timestamp
	subtaskQueue := []*subtaskEntry{}
	eg.Go(func() error {
		return w.subtasks.ReadOnly(ctx).WatchF(func(e *watch.Event) error {
			if e.Type == watch.EventError {
				return e.Err
			}

			var subtaskKey string
			subtask := Subtask{}
			if err := e.Unmarshal(&subtaskKey, &subtask); err != nil {
				return err
			}

			mutex.Lock()
			defer mutex.Unlock()

			index := -1
			for i, entry := range subtaskQueue {
				if entry.id == subtask.ID {
					index = i
					break
				}
			}

			if e.Type == watch.EventPut && subtask.State == State_RUNNING {
				if index == -1 {
					ctx, cancel := context.WithCancel(ctx)
					// TODO: insert based on task timestamp
					subtaskQueue = append(subtaskQueue, &subtaskEntry{
						id:        subtask.ID,
						timestamp: subtask.TaskTime,
						ctx:       ctx,
						cancel:    cancel,
					})
				}
			} else if index != -1 {
				// Subtask is not valid, cancel and remove it
				subtaskQueue[index].cancel()
				subtaskQueue = append(subtaskQueue[:index], subtaskQueue[index+1:]...)
			}

			cond.Signal()
			return nil
		})
	})

	eg.Go(func() error {
		for {
			if err := func() error {
				mutex.Lock()
				defer mutex.Unlock()

				// Loop through subtasks, attempt to claim one
				for _, entry := range subtaskQueue {
					if err := w.claims.Claim(ctx, entry.id, &Claim{}, func(ctx context.Context) (retErr error) {
						mutex.Unlock()
						defer mutex.Lock()

						// Read out the claimed subtask
						subtask := &Subtask{}
						if err := w.subtasks.ReadOnly(ctx).Get(entry.id, subtask); err != nil {
							if col.IsErrNotFound(err) {
								return nil
							}
							return err
						}

						if subtask.State != State_RUNNING {
							return nil
						}

						// When we return, write out the updated subtask
						defer func() {
							if _, err := col.NewSTM(ctx, w.etcdClient, func(stm col.STM) error {
								updateSubtask := &Subtask{}
								return w.subtasks.ReadWrite(stm).Update(entry.id, updateSubtask, func() error {
									if subtask.State != State_RUNNING {
										fmt.Printf("unexpected state: subtask was finished while claimed by a worker")
										return nil
									}
									updateSubtask.State = State_SUCCESS
									updateSubtask.UserData = subtask.UserData
									if retErr != nil {
										updateSubtask.State = State_FAILURE
										updateSubtask.Reason = retErr.Error()
										retErr = nil
									}
									return nil
								})
							}); retErr == nil && !col.IsErrNotFound(err) {
								retErr = err
							}
						}()

						// We need a different ctx that will be canceled if the task gets deleted
						ctx, cancel := context.WithCancel(ctx)
						go func() {
							<-entry.ctx.Done()
							cancel()
						}()
						defer entry.cancel()

						return cb(ctx, subtask.UserData)
					}); err == nil {
						// We processed a subtask - abort this loop and resume from the start of subtasks again
						return nil
					} else if err != col.ErrNotClaimed {
						return err
					}
				}

				// We looped over all known subtasks and didn't get any claims, wait for
				// something to change
				cond.Wait()
				return nil
			}(); err != nil {
				return err
			}
		}
	})

	return eg.Wait()
}

func (t *task) Run(
	ctx context.Context,
	subtaskData chan *types.Any,
	collect func(ctx context.Context, subtask *types.Any) error,
) (retErr error) {
	eg, ctx := errgroup.WithContext(ctx)
	subtaskIDs := make(chan string)

	// Remove all subtasks in case we exit early
	defer func() {
		if _, err := col.NewSTM(ctx, t.master.etcdClient, func(stm col.STM) error {
			t.master.subtasks.ReadWrite(stm).DeleteAllPrefix(t.id)
			return nil
		}); retErr == nil {
			retErr = err
		}
	}()

	eg.Go(func() error {
		for userData := range subtaskData {
			subtask := &Subtask{
				ID:       fmt.Sprintf("%s-%s", t.id, uuid.NewWithoutDashes()),
				UserData: userData,
				TaskTime: t.timestamp,
				State:    State_RUNNING,
			}

			if _, err := col.NewSTM(ctx, t.master.etcdClient, func(stm col.STM) error {
				return t.master.subtasks.ReadWrite(stm).Put(subtask.ID, subtask)
			}); err != nil {
				return err
			}

			subtaskIDs <- subtask.ID
		}
		return nil
	})

	eg.Go(func() error {
		for {
			select {
			case id := <-subtaskIDs:
				if err := t.master.subtasks.ReadOnly(ctx).WatchOneF(id, func(e *watch.Event) error {
					switch e.Type {
					case watch.EventError:
						return e.Err
					case watch.EventPut:
						var subtaskKey string
						subtask := Subtask{}
						if err := e.Unmarshal(&subtaskKey, &subtask); err != nil {
							return err
						}

						if subtask.State == State_FAILURE {
							return fmt.Errorf("%s", subtask.Reason)
						}

						// If the subtask is still running, put the ID back so we can wait again
						if subtask.State == State_RUNNING {
							subtaskIDs <- id
						}

						return nil
					case watch.EventDelete:
						return fmt.Errorf("subtask was unexpectedly deleted")
					}
					return fmt.Errorf("unrecognized watch event: %v", e.Type)
				}); err != nil {
					// Put the id back in the subtask IDs so we can remove it at the end
					subtaskIDs <- id
					return err
				}
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	})

	return eg.Wait()
}