// Copyright 2012 Brian "bojo" Jones. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

package redistore

import (
	"bytes"
	"encoding/base32"
	"encoding/gob"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-redis/redis"
	"github.com/gorilla/securecookie"
	"github.com/gorilla/sessions"
)

// Amount of time for cookies/redis keys to expire.
var sessionExpire = 86400 * 3000

// SessionSerializer provides an interface hook for alternative serializers
type SessionSerializer interface {
	Deserialize(d []byte, ss *sessions.Session) error
	Serialize(ss *sessions.Session) ([]byte, error)
	SerializeData(m map[string]interface{}) ([]byte, error)
	DeserializeData(d []byte, m*map[string]interface{}) error
}

// JSONSerializer encode the session map to JSON.
type JSONSerializer struct{}

// Serialize to JSON. Will err if there are unmarshalable key values
func (s JSONSerializer) Serialize(ss *sessions.Session) ([]byte, error) {
	m := make(map[string]interface{}, len(ss.Values))
	for k, v := range ss.Values {
		ks, ok := k.(string)
		if !ok {
			err := fmt.Errorf("Non-string key value, cannot serialize session to JSON: %v", k)
			fmt.Printf("redistore.JSONSerializer.serialize() Error: %v", err)
			return nil, err
		}
		m[ks] = v
	}
	return s.SerializeData(m)
}

func (s JSONSerializer) SerializeData(data map[string]interface{}) ([]byte, error){
	return json.Marshal(data)
}

// Deserialize back to map[string]interface{}
func (s JSONSerializer) Deserialize(d []byte, ss *sessions.Session) error {
	m := make(map[string]interface{})
	err := s.DeserializeData(d, &m)
	if err != nil {
		fmt.Printf("redistore.JSONSerializer.deserialize() Error: %v", err)
		return err
	}
	for k, v := range m {
		ss.Values[k] = v
	}
	return nil
}

func (s JSONSerializer) DeserializeData(d []byte, m*map[string]interface{}) error {
	return json.Unmarshal(d, m)
}

// GobSerializer uses gob package to encode the session map
type GobSerializer struct{}

// Serialize using gob
func (s GobSerializer) Serialize(ss *sessions.Session) ([]byte, error) {
	buf := new(bytes.Buffer)
	enc := gob.NewEncoder(buf)
	err := enc.Encode(ss.Values)
	if err == nil {
		return buf.Bytes(), nil
	}
	return nil, err
}

// Deserialize back to map[interface{}]interface{}
func (s GobSerializer) Deserialize(d []byte, ss *sessions.Session) error {
	dec := gob.NewDecoder(bytes.NewBuffer(d))
	return dec.Decode(&ss.Values)
}

// RediStore stores sessions in a redis backend.
type RediStore struct {
	Pool          redis.Cmdable
	Codecs        []securecookie.Codec
	Options       *sessions.Options // default configuration
	DefaultMaxAge int               // default Redis TTL for a MaxAge == 0 session
	maxLength     int
	keyPrefix     string
	serializer    SessionSerializer
}

type SessionInfo struct {
	ID string
	CreateTime interface{}
}

// SetMaxLength sets RediStore.maxLength if the `l` argument is greater or equal 0
// maxLength restricts the maximum length of new sessions to l.
// If l is 0 there is no limit to the size of a session, use with caution.
// The default for a new RediStore is 4096. Redis allows for max.
// value sizes of up to 512MB (http://redis.io/topics/data-types)
// Default: 4096,
func (s *RediStore) SetMaxLength(l int) {
	if l >= 0 {
		s.maxLength = l
	}
}

// SetKeyPrefix set the prefix
func (s *RediStore) SetKeyPrefix(p string) {
	s.keyPrefix = p
}

// SetSerializer sets the serializer
func (s *RediStore) SetSerializer(ss SessionSerializer) {
	s.serializer = ss
}

// SetMaxAge restricts the maximum age, in seconds, of the session record
// both in database and a browser. This is to change session storage configuration.
// If you want just to remove session use your session `s` object and change it's
// `Options.MaxAge` to -1, as specified in
//    http://godoc.org/github.com/gorilla/sessions#Options
//
// Default is the one provided by this package value - `sessionExpire`.
// Set it to 0 for no restriction.
// Because we use `MaxAge` also in SecureCookie crypting algorithm you should
// use this function to change `MaxAge` value.
func (s *RediStore) SetMaxAge(v int) {
	var c *securecookie.SecureCookie
	var ok bool
	s.Options.MaxAge = v
	for i := range s.Codecs {
		if c, ok = s.Codecs[i].(*securecookie.SecureCookie); ok {
			c.MaxAge(v)
		} else {
			fmt.Printf("Can't change MaxAge on codec %v\n", s.Codecs[i])
		}
	}
}

func dial(network, address, password string,db int) (redis.Cmdable, error) {
	c:= redis.NewClient(&redis.Options{
		Network:           network,
		Addr:               address,
		Password:           password,
		DB:                 db,
		MaxRetries:         2,
		DialTimeout:       time.Second*2,
		ReadTimeout:        time.Second*2,
		WriteTimeout:       time.Second*2,
		PoolSize:           100,
		MinIdleConns:       1,
		MaxConnAge:         time.Minute*5,
	})
	err:=c.Ping().Err()
	if err!=nil{
		return nil,err
	}

	return c, nil
}

// NewRediStore returns a new RedisStore.
// size: maximum number of idle connections.
func NewRedisStore(size int, network, address, password string,db int, keyPairs ...[]byte) (*RediStore, error) {
	pool,err:=dialWithDB(network, address, password,db)
	if err!=nil{
		return nil,err
	}
	return NewRedisStoreWithPool(pool, keyPairs...),nil
}

func dialWithDB(network, address, password string,DB int) (redis.Cmdable, error) {
	c, err := dial(network, address, password,DB)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// NewRediStoreWithPool instantiates a RediStore with a *redis.Pool passed in.
func NewRedisStoreWithPool(pool redis.Cmdable, keyPairs ...[]byte) *RediStore {
	rs := &RediStore{
		// http://godoc.org/github.com/gomodule/redigo/redis#Pool
		Pool:   pool,
		Codecs: securecookie.CodecsFromPairs(keyPairs...),
		Options: &sessions.Options{
			Path:   "/",
			MaxAge: sessionExpire,
		},
		DefaultMaxAge: 60 * 20, // 20 minutes seems like a reasonable default
		maxLength:     4096,
		keyPrefix:     "session_",
		serializer:    JSONSerializer{},
	}
	return rs
}

// Close closes the underlying *redis.Pool
func (s *RediStore) Close() error {
	return s.Pool.Shutdown().Err()
}

// Get returns a session for the given name after adding it to the registry.
//
// See gorilla/sessions FilesystemStore.Get().
func (s *RediStore) Get(r *http.Request, name string) (*sessions.Session, error) {
	return sessions.GetRegistry(r).Get(s, name)
}

// New returns a session for the given name without adding it to the registry.
//
// See gorilla/sessions FilesystemStore.New().
func (s *RediStore) New(r *http.Request, name string) (*sessions.Session, error) {
	var (
		err error
		ok  bool
	)
	session := sessions.NewSession(s, name)
	// make a copy
	options := *s.Options
	session.Options = &options
	session.IsNew = true
	if c, errCookie := r.Cookie(name); errCookie == nil {
		sessionInfo:=&SessionInfo{}
		err = securecookie.DecodeMulti(name, c.Value, sessionInfo, s.Codecs...)
		if err == nil {
			session.ID = sessionInfo.ID
			ok, err = s.load(session)
			if err==nil && ok {
				createdTimeV:=session.Values["created_time"]
				if createdTimeV!=sessionInfo.CreateTime{
					session.Values = map[interface{}]interface{}{}
					session.IsNew = true
				}else{
					session.IsNew = false
				}
			}else{
				session.IsNew = !(err == nil && ok) // not new if no error and data available
			}
		}
	}
	return session, err
}

// Save adds a single session to the response.
func (s *RediStore) Save(r *http.Request, w http.ResponseWriter, session *sessions.Session) error {
	// Marked for deletion.
	if session.Options.MaxAge <= 0 {
		if err := s.delete(session); err != nil {
			return err
		}
		http.SetCookie(w, sessions.NewCookie(session.Name(), "", session.Options))
	} else {
		// Build an alphanumeric key for the redis store.
		if session.ID == "" {
			session.ID = strings.TrimRight(base32.StdEncoding.EncodeToString(securecookie.GenerateRandomKey(32)), "=")
		}
		createdTime,ok:=session.Values["created_time"]
		if !ok {
			createdTime = time.Now().Format("20060102150405")
			session.Values["created_time"] = createdTime
		}
		if err := s.save(session); err != nil {
			return err
		}
		encoded, err := securecookie.EncodeMulti(session.Name(), &SessionInfo{ID:session.ID,CreateTime:createdTime}, s.Codecs...)
		if err != nil {
			return err
		}

		http.SetCookie(w, sessions.NewCookie(session.Name(), encoded, session.Options))
	}
	return nil
}

// Delete removes the session from redis, and sets the cookie to expire.
//
// WARNING: This method should be considered deprecated since it is not exposed via the gorilla/sessions interface.
// Set session.Options.MaxAge = -1 and call Save instead. - July 18th, 2013
func (s *RediStore) Delete(r *http.Request, w http.ResponseWriter, session *sessions.Session) error {

	if  err := s.Pool.Del( s.keyPrefix+session.ID).Err(); err != nil {
		return err
	}
	// Set cookie to expire.
	options := *session.Options
	options.MaxAge = -1
	http.SetCookie(w, sessions.NewCookie(session.Name(), "", &options))
	// Clear session values.
	for k := range session.Values {
		delete(session.Values, k)
	}
	return nil
}

// save stores the session in redis.
func (s *RediStore) save(session *sessions.Session) error {
	b, err := s.serializer.Serialize(session)
	if err != nil {
		return err
	}
	if s.maxLength != 0 && len(b) > s.maxLength {
		return errors.New("SessionStore: the value to store is too big")
	}

	age := session.Options.MaxAge
	if age == 0 {
		age = s.DefaultMaxAge
	}
	return s.Pool.Set( s.keyPrefix+session.ID, b,time.Duration(age)*time.Second ).Err()
}

func (s *RediStore)Store(ID string,data map[string]interface{})error  {
	b, err := s.serializer.SerializeData(data)
	if err != nil {
		return err
	}
	if s.maxLength != 0 && len(b) > s.maxLength {
		return errors.New("SessionStore: the value to store is too big")
	}
	return s.Pool.Set( s.keyPrefix+ID, b,time.Duration(sessionExpire)*time.Second ).Err()
}

func (s *RediStore)Load(ID string,data*map[string]interface{})(bool, error)   {
	d,err:=s.Pool.Get(s.keyPrefix+ID).Bytes()
	if err != nil {
		if err ==redis.Nil {
			return false,nil
		}
		return false, err
	}
	if data == nil {
		return false, nil // no data was associated with this key
	}
	return true, s.serializer.DeserializeData(d, data)
}

// load reads the session from redis.
// returns true if there is a sessoin data in DB
func (s *RediStore) load(session *sessions.Session) (bool, error) {
	data,err:=s.Pool.Get(s.keyPrefix+session.ID).Bytes()
	if err != nil {
		if err ==redis.Nil {
			return false,nil
		}
		return false, err
	}
	if data == nil {
		return false, nil // no data was associated with this key
	}
	return true, s.serializer.Deserialize(data, session)
}

// delete removes keys from redis if MaxAge<0
func (s *RediStore) delete(session *sessions.Session) error {
	return  s.Pool.Del(s.keyPrefix+session.ID).Err()
}
