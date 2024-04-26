package redis

import (
	"context"
	"sync"
	"time"

	"github.com/go-session/session/v3"
	jsoniter "github.com/json-iterator/go"
	"github.com/redis/go-redis/v9"
)

var (
	_             session.ManagerStore = &managerStore{}
	_             session.Store        = &store{}
	jsonMarshal                        = jsoniter.Marshal
	jsonUnmarshal                      = jsoniter.Unmarshal
)

// NewRedisStore create an instance of a redis store
func NewRedisStore(opts *Options, prefix ...string) session.ManagerStore {
	if opts == nil {
		panic("options cannot be nil")
	}
	return NewRedisStoreWithCli(
		redis.NewClient(opts.redisOptions()),
		prefix...,
	)
}

// NewRedisStoreWithCli create an instance of a redis store
func NewRedisStoreWithCli(cli *redis.Client, prefix ...string) session.ManagerStore {
	store := &managerStore{
		cli: cli,
	}
	if len(prefix) > 0 {
		store.prefix = prefix[0]
	}
	return store
}

// NewRedisClusterStore create an instance of a redis cluster store
func NewRedisClusterStore(opts *ClusterOptions, prefix ...string) session.ManagerStore {
	if opts == nil {
		panic("options cannot be nil")
	}
	return NewRedisClusterStoreWithCli(
		redis.NewClusterClient(opts.redisClusterOptions()),
		prefix...,
	)
}

// NewRedisClusterStoreWithCli create an instance of a redis cluster store
func NewRedisClusterStoreWithCli(cli *redis.ClusterClient, prefix ...string) session.ManagerStore {
	store := &managerStore{
		cli: cli,
	}
	if len(prefix) > 0 {
		store.prefix = prefix[0]
	}
	return store
}

type clienter interface {
	Get(ctx context.Context, key string) *redis.StringCmd
	Set(ctx context.Context, key string, value interface{}, expiration time.Duration) *redis.StatusCmd
	Expire(ctx context.Context, key string, expiration time.Duration) *redis.BoolCmd
	Exists(ctx context.Context, keys ...string) *redis.IntCmd
	TxPipeline() redis.Pipeliner
	Del(ctx context.Context, keys ...string) *redis.IntCmd
	Close() error
}

type managerStore struct {
	cli    clienter
	prefix string
}

func (s *managerStore) getKey(key string) string {
	return s.prefix + key
}

func (s *managerStore) getValue(ctx context.Context, sid string) (string, error) {
	cmd := s.cli.Get(ctx, s.getKey(sid))
	if err := cmd.Err(); err != nil {
		if err == redis.Nil {
			return "", nil
		}
		return "", err
	}

	return cmd.Val(), nil
}

func (s *managerStore) parseValue(value string) (map[string]interface{}, error) {
	var values map[string]interface{}
	if len(value) > 0 {
		err := jsonUnmarshal([]byte(value), &values)
		if err != nil {
			return nil, err
		}
	}
	return values, nil
}

func (s *managerStore) Create(ctx context.Context, sid string, expired int64) (session.Store, error) {
	return newStore(ctx, s, sid, expired, nil), nil
}

func (s *managerStore) Update(ctx context.Context, sid string, expired int64) (session.Store, error) {
	value, err := s.getValue(ctx, sid)
	if err != nil {
		return nil, err
	} else if value == "" {
		return newStore(ctx, s, sid, expired, nil), nil
	}

	cmd := s.cli.Expire(ctx, s.getKey(sid), time.Duration(expired)*time.Second)
	if err = cmd.Err(); err != nil {
		return nil, err
	}

	values, err := s.parseValue(value)
	if err != nil {
		return nil, err
	}

	return newStore(ctx, s, sid, expired, values), nil
}

func (s *managerStore) Delete(ctx context.Context, sid string) error {
	if ok, err := s.Check(ctx, sid); err != nil {
		return err
	} else if !ok {
		return nil
	}

	cmd := s.cli.Del(ctx, s.getKey(sid))
	return cmd.Err()
}

func (s *managerStore) Check(ctx context.Context, sid string) (bool, error) {
	cmd := s.cli.Exists(ctx, s.getKey(sid))
	if err := cmd.Err(); err != nil {
		return false, err
	}
	return cmd.Val() > 0, nil
}

func (s *managerStore) Refresh(ctx context.Context, oldsid, sid string, expired int64) (session.Store, error) {
	value, err := s.getValue(ctx, oldsid)
	if err != nil {
		return nil, err
	} else if value == "" {
		return newStore(ctx, s, sid, expired, nil), nil
	}

	pipe := s.cli.TxPipeline()
	pipe.Set(ctx, s.getKey(sid), value, time.Duration(expired)*time.Second)
	pipe.Del(ctx, s.getKey(oldsid))
	_, err = pipe.Exec(ctx)
	if err != nil {
		return nil, err
	}

	values, err := s.parseValue(value)
	if err != nil {
		return nil, err
	}

	return newStore(ctx, s, sid, expired, values), nil
}

func (s *managerStore) Close() error {
	return s.cli.Close()
}

func newStore(ctx context.Context, ms *managerStore, sid string, expired int64, values map[string]interface{}) *store {
	if values == nil {
		values = make(map[string]interface{})
	}

	return &store{
		ms:      ms,
		ctx:     ctx,
		sid:     sid,
		expired: expired,
		values:  values,
	}
}

type store struct {
	sync.RWMutex
	ms      *managerStore
	ctx     context.Context
	sid     string
	expired int64
	values  map[string]interface{}
}

func (s *store) Context() context.Context {
	return s.ctx
}

func (s *store) SessionID() string {
	return s.sid
}

func (s *store) Set(key string, value interface{}) {
	s.Lock()
	s.values[key] = value
	s.Unlock()
}

func (s *store) Get(key string) (interface{}, bool) {
	s.RLock()
	defer s.RUnlock()
	val, ok := s.values[key]
	return val, ok
}

func (s *store) Delete(key string) interface{} {
	s.RLock()
	v, ok := s.values[key]
	s.RUnlock()
	if ok {
		s.Lock()
		delete(s.values, key)
		s.Unlock()
	}
	return v
}

func (s *store) Flush() error {
	s.Lock()
	s.values = make(map[string]interface{})
	s.Unlock()
	return s.Save()
}

func (s *store) Save() error {
	var value string

	s.RLock()
	if len(s.values) > 0 {
		buf, err := jsonMarshal(s.values)
		if err != nil {
			s.RUnlock()
			return err
		}
		value = string(buf)
	}
	s.RUnlock()

	cmd := s.ms.cli.Set(s.ctx, s.ms.getKey(s.sid), value, time.Duration(s.expired)*time.Second)
	return cmd.Err()
}
