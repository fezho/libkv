package zookeeper

import (
	"strings"
	"time"

	"github.com/fezho/libkv"
	"github.com/fezho/libkv/store"
	zk "github.com/samuel/go-zookeeper/zk"
)

const (
	// SOH control character
	SOH            = "\x01"
	defaultTimeout = 10 * time.Second
	syncRetryLimit = 5
)

// Zookeeper is the receiver type for
// the Store interface
type Zookeeper struct {
	timeout time.Duration
	client  *zk.Conn
}

type zookeeperLock struct {
	client *zk.Conn
	lock   *zk.Lock
	key    string
	value  []byte
}

// Register registers zookeeper to libkv
func Register() {
	libkv.AddStore(store.ZK, New)
}

// New creates a new Zookeeper client given a
// list of endpoints and an optional tls config
func New(endpoints []string, options *store.Config) (store.Store, error) {
	s := &Zookeeper{timeout: defaultTimeout}

	// Set options
	if options != nil {
		if options.ConnectionTimeout != 0 {
			s.setTimeout(options.ConnectionTimeout)
		}
	}

	// Connect to Zookeeper
	conn, _, err := zk.Connect(endpoints, s.timeout)
	if err != nil {
		return nil, err
	}
	s.client = conn

	return s, nil
}

// setTimeout sets the timeout for connecting to Zookeeper
func (s *Zookeeper) setTimeout(time time.Duration) {
	s.timeout = time
}

// Get the value at "key", returns the last modified index
// to use in conjunction to Atomic calls
func (s *Zookeeper) Get(key string) (*store.KVPair, error) {
	nkey := s.normalize(key)
	resp, meta, _, err := s.getWithSyncRetry(nkey, false)
	if err != nil {
		return nil, err
	}

	return &store.KVPair{
		Key:       key,
		Value:     resp,
		LastIndex: uint64(meta.Version),
	}, nil
}

// Put a value at "key"
func (s *Zookeeper) Put(key string, value []byte, opts *store.WriteOptions) error {
	exists, err := s.Exists(key)
	if err != nil {
		return err
	}

	if exists {
		_, err = s.client.Set(s.normalize(key), value, -1)
		return err
	}

	ephemeral := false
	if opts != nil && opts.TTL > 0 {
		ephemeral = true
	}

	err = s.createFullPath(store.SplitKey(strings.TrimSuffix(key, "/")), value, ephemeral)
	if err == zk.ErrNodeExists {
		_, err = s.client.Set(s.normalize(key), value, -1)
		return err
	}
	return err
}

// Delete a value at "key"
func (s *Zookeeper) Delete(key string) error {
	err := s.client.Delete(s.normalize(key), -1)
	if err == zk.ErrNoNode {
		return store.ErrKeyNotFound
	}
	return err
}

// Exists checks if the key exists inside the store
func (s *Zookeeper) Exists(key string) (bool, error) {
	exists, _, err := s.client.Exists(s.normalize(key))
	if err != nil {
		return false, err
	}
	return exists, nil
}

// Watch for changes on a "key"
// It returns a channel that will receive changes or pass
// on errors. Upon creation, the current value will first
// be sent to the channel. Providing a non-nil stopCh can
// be used to stop watching.
func (s *Zookeeper) Watch(key string, stopCh <-chan struct{}) (<-chan *store.KVPair, error) {
	nkey := s.normalize(key)
	// GetW the key first, if there's error return directly
	resp, meta, eventCh, err := s.getWithSyncRetry(nkey, true)
	if err != nil {
		return nil, err
	}

	// Catch zk notifications and fire changes into the channel.
	watchCh := make(chan *store.KVPair)
	go func() {
		defer close(watchCh)

		var fireEvt = true
		for {
			if fireEvt {
				watchCh <- &store.KVPair{
					Key:       key,
					Value:     resp,
					LastIndex: uint64(meta.Version),
				}
			}

			select {
			case e := <-eventCh:
				// Only fire an event if the data in the node changed.
				fireEvt = e.Type == zk.EventNodeDataChanged
				// TODO: if EventNodeDeleted, return nil???
			case <-stopCh:
				// There is no way to stop GetW so just quit
				return
			}

			resp, meta, eventCh, err = s.getWithSyncRetry(nkey, true)
			if err != nil {
				return
			}
		}
	}()

	return watchCh, nil
}

// WatchTree watches for changes on a "directory"
// It returns a channel that will receive changes or pass
// on errors. Upon creating a watch, the current childs values
// will be sent to the channel .Providing a non-nil stopCh can
// be used to stop watching.
func (s *Zookeeper) WatchTree(directory string, stopCh <-chan struct{}) (<-chan []*store.KVPair, error) {
	ndirectory := s.normalize(directory)
	// GetW the keys first, if there's error return directly
	keys, _, eventCh, err := s.client.ChildrenW(s.normalize(directory))
	if err != nil {
		return nil, err
	}

	// Catch zk notifications and fire changes into the channel.
	watchCh := make(chan []*store.KVPair)
	go func() {
		defer close(watchCh)

		var fireEvt = true
		for {
			if fireEvt {
				kvs, err := s.getListWithPath(directory, keys)
				if err != nil {
					// Failed to get values for one or more of the keys,
					// the list may be out of date so try again.
					goto WATCH
				}
				watchCh <- kvs
			}

			select {
			case e := <-eventCh:
				// Only fire an event if the children have changed.
				fireEvt = e.Type == zk.EventNodeChildrenChanged
			case <-stopCh:
				// There is no way to stop GetW so just quit
				return
			}

		WATCH:
			keys, _, eventCh, err = s.client.ChildrenW(ndirectory)
			if err != nil {
				return
			}
		}
	}()

	return watchCh, nil
}

// List child nodes of a given directory
func (s *Zookeeper) List(directory string) ([]*store.KVPair, error) {
	children := make([]string, 0)
	err := s.listChildrenRecursive(&children, directory)
	if err != nil {
		return nil, err
	}

	kvs, err := s.getList(children)
	if err != nil {
		// If node is not found: List is out of date, retry
		if err == store.ErrKeyNotFound {
			return s.List(directory)
		}
		return nil, err
	}

	return kvs, nil
}

// DeleteTree deletes a range of keys under a given directory
func (s *Zookeeper) DeleteTree(directory string) error {
	children, err := s.listChildren(directory)
	if err != nil {
		return err
	}

	var reqs []interface{}

	for _, c := range children {
		reqs = append(reqs, &zk.DeleteRequest{
			Path:    s.normalize(directory + "/" + c),
			Version: -1,
		})
	}

	_, err = s.client.Multi(reqs...)
	return err
}

// AtomicPut put a value at "key" if the key has not been
// modified in the meantime, throws an error if this is the case
func (s *Zookeeper) AtomicPut(key string, value []byte, previous *store.KVPair, _ *store.WriteOptions) (bool, *store.KVPair, error) {
	var lastIndex uint64
	nkey := s.normalize(key)

	if previous != nil {
		meta, err := s.client.Set(nkey, value, int32(previous.LastIndex))
		if err != nil {
			// Compare Failed
			if err == zk.ErrBadVersion {
				return false, nil, store.ErrKeyModified
			}
			return false, nil, err
		}
		lastIndex = uint64(meta.Version)
	} else {
		// Interpret previous == nil as create operation.
		_, err := s.client.Create(nkey, value, 0, zk.WorldACL(zk.PermAll))
		if err != nil {
			// Directory does not exist
			if err == zk.ErrNoNode {
				// Create the directory
				parts := store.SplitKey(strings.TrimSuffix(key, "/"))
				if err = s.createFullPath(parts, value, false); err != nil {
					// Node exist error (when previous nil)
					if err == zk.ErrNodeExists {
						return false, nil, store.ErrKeyExists
					}
					return false, nil, err
				}
			} else {
				// Node Exists error (when previous nil)
				if err == zk.ErrNodeExists {
					return false, nil, store.ErrKeyExists
				}

				// Unhandled error
				return false, nil, err
			}
		}
		lastIndex = 0 // Newly created nodes have version 0.
	}

	pair := &store.KVPair{
		Key:       key,
		Value:     value,
		LastIndex: lastIndex,
	}

	return true, pair, nil
}

// AtomicDelete deletes a value at "key" if the key
// has not been modified in the meantime, throws an
// error if this is the case
func (s *Zookeeper) AtomicDelete(key string, previous *store.KVPair) (bool, error) {
	if previous == nil {
		return false, store.ErrPreviousNotSpecified
	}

	err := s.client.Delete(s.normalize(key), int32(previous.LastIndex))
	if err != nil {
		// Key not found
		if err == zk.ErrNoNode {
			return false, store.ErrKeyNotFound
		}
		// Compare failed
		if err == zk.ErrBadVersion {
			return false, store.ErrKeyModified
		}
		// General store error
		return false, err
	}
	return true, nil
}

// NewLock returns a handle to a lock struct which can
// be used to provide mutual exclusion on a key
func (s *Zookeeper) NewLock(key string, options *store.LockOptions) (lock store.Locker, err error) {
	value := []byte("")

	// Apply options
	if options != nil {
		if options.Value != nil {
			value = options.Value
		}
	}

	nkey := s.normalize(key)
	lock = &zookeeperLock{
		client: s.client,
		key:    nkey,
		value:  value,
		lock:   zk.NewLock(s.client, nkey, zk.WorldACL(zk.PermAll)),
	}

	return lock, err
}

// Lock attempts to acquire the lock and blocks while
// doing so. It returns a channel that is closed if our
// lock is lost or if an error occurs
func (l *zookeeperLock) Lock(stopChan chan struct{}) (<-chan struct{}, error) {
	err := l.lock.Lock()

	lostCh := make(chan struct{})
	if err == nil {
		// We hold the lock, we can set our value
		_, err = l.client.Set(l.key, l.value, -1)
		if err == nil {
			go l.monitorLock(stopChan, lostCh)
		}
	}

	return lostCh, err
}

// Unlock the "key". Calling unlock while
// not holding the lock will throw an error
func (l *zookeeperLock) Unlock() error {
	return l.lock.Unlock()
}

// Close closes the client connection
func (s *Zookeeper) Close() {
	s.client.Close()
}

// Normalize the key for usage in Zookeeper
func (s *Zookeeper) normalize(key string) string {
	key = store.Normalize(key)
	return strings.TrimSuffix(key, "/")
}

// getWithSyncRetry re-sync few times if get SOH or empty string
// caused by creating and writing znodes non-atomically.
func (s *Zookeeper) getWithSyncRetry(normalizedKey string, watch bool) (resp []byte, meta *zk.Stat, ech <-chan zk.Event, err error) {
	for i := 0; i <= syncRetryLimit; i++ {
		if watch {
			resp, meta, ech, err = s.client.GetW(normalizedKey)
		} else {
			resp, meta, err = s.client.Get(normalizedKey)
		}
		if err != nil {
			if err == zk.ErrNoNode {
				err = store.ErrKeyNotFound
			}
			return
		}

		if string(resp) != SOH && string(resp) != "" {
			return
		}

		if i < syncRetryLimit {
			if _, err = s.client.Sync(normalizedKey); err != nil {
				return
			}
		}
	}
	return
}

// createFullPath creates the entire path for a directory
// that does not exist and sets the value of the last znode to data
func (s *Zookeeper) createFullPath(path []string, data []byte, ephemeral bool) error {
	for i := 1; i <= len(path); i++ {
		newpath := "/" + strings.Join(path[:i], "/")

		// create leaf node
		if i == len(path) {
			flag := int32(0)
			if ephemeral {
				flag = zk.FlagEphemeral
			}
			_, err := s.client.Create(newpath, data, flag, zk.WorldACL(zk.PermAll))
			return err
		}

		_, err := s.client.Create(newpath, data, 0, zk.WorldACL(zk.PermAll))
		if err != nil {
			// Skip if node already exists in non-leaf node
			if err != zk.ErrNodeExists {
				return err
			}
		}
	}
	return nil
}

// getListWithPath gets the key/value pairs for a list of keys under
// a given path.
//
// This is generally used when we get a list of child keys which
// are stripped out of their path (for example when using ChildrenW).
func (s *Zookeeper) getListWithPath(path string, keys []string) ([]*store.KVPair, error) {
	kvs := []*store.KVPair{}

	for _, key := range keys {
		pair, err := s.Get(strings.TrimSuffix(path, "/") + s.normalize(key))
		if err != nil {
			return nil, err
		}

		kvs = append(kvs, &store.KVPair{
			Key:       key,
			Value:     pair.Value,
			LastIndex: pair.LastIndex,
		})
	}

	return kvs, nil
}

// listChildrenRecursive lists the children of a directory as well as
// all the descending children from sub-folders in a recursive fashion.
func (s *Zookeeper) listChildrenRecursive(list *[]string, directory string) error {
	children, err := s.listChildren(directory)
	if err != nil {
		return err
	}

	// We reached a leaf.
	if len(children) == 0 {
		return nil
	}

	for _, c := range children {
		c = strings.TrimSuffix(directory, "/") + "/" + c
		err := s.listChildrenRecursive(list, c)
		if err != nil && err != zk.ErrNoChildrenForEphemerals {
			return err
		}
		*list = append(*list, c)
	}

	return nil
}

// listChildren lists the direct children of a directory
func (s *Zookeeper) listChildren(directory string) ([]string, error) {
	children, _, err := s.client.Children(s.normalize(directory))
	if err != nil {
		if err == zk.ErrNoNode {
			return nil, store.ErrKeyNotFound
		}
		return nil, err
	}
	return children, nil
}

// getList returns key/value pairs from a list of keys.
//
// This is generally used when we have a full list of keys with
// their full path included.
func (s *Zookeeper) getList(keys []string) ([]*store.KVPair, error) {
	kvs := []*store.KVPair{}

	for _, key := range keys {
		pair, err := s.Get(strings.TrimSuffix(key, "/"))
		if err != nil {
			return nil, err
		}

		kvs = append(kvs, &store.KVPair{
			Key:       key,
			Value:     pair.Value,
			LastIndex: pair.LastIndex,
		})
	}

	return kvs, nil
}

func (l *zookeeperLock) monitorLock(stopCh <-chan struct{}, lostCh chan struct{}) {
	defer close(lostCh)

	for {
		_, _, eventCh, err := l.client.GetW(l.key)
		if err != nil {
			// We failed to set watch, relinquish the lock
			return
		}
		select {
		case e := <-eventCh:
			if e.Type == zk.EventNotWatching ||
				(e.Type == zk.EventSession && e.State == zk.StateExpired) {
				// Either the session has been closed and our watch has been
				// invalidated or the session has expired.
				return
			} else if e.Type == zk.EventNodeDataChanged {
				// Somemone else has written to the lock node and believes
				// that they have the lock.
				return
			}
		case <-stopCh:
			// The caller has requested that we relinquish our lock
			return
		}
	}
}
