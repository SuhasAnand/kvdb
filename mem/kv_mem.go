package mem

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/portworx/kvdb"
)

const (
	// Name is the name of this kvdb implementation.
	Name = "kv-mem"
)

func init() {
	if err := kvdb.Register(Name, New); err != nil {
		panic(err.Error())
	}
}

type memKV struct {
	m      map[string]*kvdb.KVPair
	w      map[string]*watchData
	wt     map[string]*watchData
	mutex  sync.Mutex
	index  uint64
	domain string
}

type watchData struct {
	cb     kvdb.WatchCB
	opaque interface{}
}

// New constructs a new kvdb.Kvdb.
func New(
	domain string,
	machines []string,
) (kvdb.Kvdb, error) {
	if domain != "" && !strings.HasSuffix(domain, "/") {
		domain = domain + "/"
	}
	return &memKV{
		m:      make(map[string]*kvdb.KVPair),
		w:      make(map[string]*watchData),
		wt:     make(map[string]*watchData),
		domain: domain,
	}, nil
}

func (kv *memKV) String() string {
	return Name
}

func (kv *memKV) Get(key string) (*kvdb.KVPair, error) {

	key = kv.domain + key
	v, ok := kv.m[key]
	if !ok {
		return nil, kvdb.ErrNotFound
	}
	return v, nil
}

func (kv *memKV) Put(
	key string,
	value interface{},
	ttl uint64,
) (*kvdb.KVPair, error) {

	var kvp *kvdb.KVPair

	key = kv.domain + key
	kv.mutex.Lock()
	defer kv.mutex.Unlock()

	index := atomic.AddUint64(&kv.index, 1)
	if ttl != 0 {
		time.AfterFunc(time.Second*time.Duration(ttl), func() {
			// TODO: handle error
			_, _ = kvdb.Instance().Delete(key)
		})
	}
	b, err := kv.toBytes(value)
	if err != nil {
		return nil, err
	}
	if old, ok := kv.m[key]; ok {
		old.Value = b
		old.Action = kvdb.KVSet
		old.ModifiedIndex = index
		old.KVDBIndex = index
		kvp = old

	} else {
		kvp = &kvdb.KVPair{
			Key:           key,
			Value:         b,
			TTL:           int64(ttl),
			KVDBIndex:     index,
			ModifiedIndex: index,
			CreatedIndex:  index,
			Action:        kvdb.KVCreate,
		}
		kv.m[key] = kvp
	}

	kv.normalize(kvp)
	go kv.fireCB(key, kvp, nil)
	return kvp, nil
}

func (kv *memKV) GetVal(key string, v interface{}) (*kvdb.KVPair, error) {
	kvp, err := kv.Get(key)
	if err != nil {
		return nil, err
	}

	err = json.Unmarshal(kvp.Value, v)
	return kvp, err
}

func (kv *memKV) Create(
	key string,
	value interface{},
	ttl uint64) (*kvdb.KVPair, error) {

	result, err := kv.Get(key)
	if err != nil {
		return kv.Put(key, value, ttl)
	}
	return result, kvdb.ErrExist
}

func (kv *memKV) Update(
	key string,
	value interface{},
	ttl uint64) (*kvdb.KVPair, error) {

	if _, err := kv.Get(key); err != nil {
		return nil, kvdb.ErrNotFound
	}
	return kv.Put(key, value, ttl)
}

func (kv *memKV) Enumerate(prefix string) (kvdb.KVPairs, error) {
	var kvp = make(kvdb.KVPairs, 0, 100)
	prefix = kv.domain + prefix

	for k, v := range kv.m {
		if strings.HasPrefix(k, prefix) && !strings.Contains(k, "/_") {
			kvpLocal := *v
			kv.normalize(&kvpLocal)
			kvp = append(kvp, &kvpLocal)
		}
	}

	return kvp, nil
}

func (kv *memKV) Delete(key string) (*kvdb.KVPair, error) {
	kvp, err := kv.Get(key)
	if err != nil {
		return nil, err
	}
	kvp.KVDBIndex = atomic.AddUint64(&kv.index, 1)
	kvp.ModifiedIndex = kvp.KVDBIndex
	kvp.Action = kvdb.KVDelete
	go kv.fireCB(kv.domain+key, kvp, nil)
	delete(kv.m, kv.domain+key)
	return kvp, nil
}

func (kv *memKV) DeleteTree(prefix string) error {

	kvp, err := kv.Enumerate(prefix)
	if err != nil {
		return err
	}
	for _, v := range kvp {
		// TODO: multiple errors
		if _, iErr := kv.Delete(v.Key); iErr != nil {
			err = iErr
		}
	}
	return err
}

func (kv *memKV) Keys(prefix, key string) ([]string, error) {
	return nil, kvdb.ErrNotSupported
}

func (kv *memKV) CompareAndSet(
	kvp *kvdb.KVPair,
	flags kvdb.KVFlags,
	prevValue []byte) (*kvdb.KVPair, error) {

	kv.mutex.Lock()

	if flags == kvdb.KVModifiedIndex {
		if kvp.ModifiedIndex != kv.index {
			kv.mutex.Unlock()
			return nil, kvdb.ErrValueMismatch
		}
	}

	result, err := kv.Get(kvp.Key)
	if err != nil {
		kv.mutex.Unlock()
		return nil, err
	} else if prevValue != nil {
		if !bytes.Equal(result.Value, prevValue) {
			kv.mutex.Unlock()
			return nil, kvdb.ErrValueMismatch
		}
	}
	kv.mutex.Unlock()
	return kv.Put(kvp.Key, kvp.Value, 0)
}

func (kv *memKV) CompareAndDelete(kvp *kvdb.KVPair, flags kvdb.KVFlags) (*kvdb.KVPair, error) {
	if flags != kvdb.KVFlags(0) {
		return nil, kvdb.ErrNotSupported
	}
	if result, err := kv.Get(kvp.Key); err != nil {
		return nil, err
	} else if !bytes.Equal(result.Value, kvp.Value) {
		return nil, kvdb.ErrNotFound
	}
	return kv.Delete(kvp.Key)
}

func (kv *memKV) WatchKey(
	key string,
	waitIndex uint64,
	opaque interface{},
	cb kvdb.WatchCB) error {
	kv.mutex.Lock()
	defer kv.mutex.Unlock()
	key = kv.domain + key
	if _, ok := kv.w[key]; ok {
		return kvdb.ErrExist
	}
	kv.w[key] = &watchData{cb: cb, opaque: opaque}
	return nil
}

func (kv *memKV) WatchTree(
	prefix string,
	waitIndex uint64,
	opaque interface{},
	cb kvdb.WatchCB) error {

	kv.mutex.Lock()
	defer kv.mutex.Unlock()
	prefix = kv.domain + prefix
	if _, ok := kv.wt[prefix]; ok {
		return kvdb.ErrExist
	}
	kv.wt[prefix] = &watchData{cb: cb, opaque: opaque}
	return nil
}

func (kv *memKV) Lock(key string, ttl uint64) (*kvdb.KVPair, error) {
	key = kv.domain + key
	duration := time.Duration(math.Min(float64(time.Second),
		float64((time.Duration(ttl)*time.Second)/10)))

	result, err := kv.Create(key, []byte("locked"), ttl)
	for err != nil {
		time.Sleep(duration)
		result, err = kv.Create(key, []byte("locked"), ttl)
	}

	if err != nil {
		return nil, err
	}
	return result, err
}

func (kv *memKV) Unlock(kvp *kvdb.KVPair) error {
	_, err := kv.CompareAndDelete(kvp, kvdb.KVFlags(0))
	return err
}

func (kv *memKV) TxNew() (kvdb.Tx, error) {
	return nil, kvdb.ErrNotSupported
}

func (kv *memKV) normalize(kvp *kvdb.KVPair) {
	kvp.Key = strings.TrimPrefix(kvp.Key, kv.domain)
}

func (kv *memKV) fireCB(key string, kvp *kvdb.KVPair, err error) {
	for k, v := range kv.w {
		if k == key {
			err := v.cb(key, v.opaque, kvp, err)
			if err != nil {
				// TODO: handle error
				_ = v.cb("", v.opaque, nil, kvdb.ErrWatchStopped)
				delete(kv.w, key)

			}
			return
		}
	}
	for k, v := range kv.wt {
		if strings.HasPrefix(key, k) {
			err := v.cb(key, v.opaque, kvp, err)
			if err != nil {
				// TODO: handle error
				_ = v.cb("", v.opaque, nil, kvdb.ErrWatchStopped)
				delete(kv.wt, key)
			}
		}
	}
}

func (kv *memKV) toBytes(val interface{}) ([]byte, error) {
	var b []byte
	var err error
	switch val.(type) {
	case string:
		s := val.(string)
		b = []byte(s)
	case []byte:
		b = make([]byte, len(val.([]byte)))
		copy(b, val.([]byte))
	default:
		b, err = json.Marshal(val)
		if err != nil {
			return nil, err
		}
	}
	return b, nil
}
