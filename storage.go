package osinredis

import (
	"bytes"
	"encoding/gob"
	"fmt"

	"github.com/RangelReale/osin"
	"github.com/garyburd/redigo/redis"
	"github.com/pkg/errors"
	"github.com/satori/go.uuid"
)

func init() {
	gob.Register(map[string]interface{}{})
	gob.Register(&osin.DefaultClient{})
	gob.Register(osin.AuthorizeData{})
	gob.Register(osin.AccessData{})
}

// Storage implements "github.com/RangelReale/osin".Storage
type Storage struct {
	pool      *redis.Pool
	keyPrefix string
}

// New initializes and returns a new Storage
func New(pool *redis.Pool, keyPrefix string) *Storage {
	return &Storage{
		pool:      pool,
		keyPrefix: keyPrefix,
	}
}

// Clone the storage if needed. For example, using mgo, you can clone the session with session.Clone
// to avoid concurrent access problems.
// This is to avoid cloning the connection at each method access.
// Can return itself if not a problem.
func (s *Storage) Clone() osin.Storage {
	return s
}

// Close the resources the Storage potentially holds (using Clone for example)
func (s *Storage) Close() {
	s.pool.Close()
}

// CreateClient inserts a new client
func (s *Storage) CreateClient(client osin.Client) error {
	conn := s.pool.Get()
	defer conn.Close()

	payload, err := encode(client)
	if err != nil {
		return errors.Wrap(err, "failed to encode client")
	}

	_, err = conn.Do("SET", s.makeKey("client", client.GetId()), payload)
	return errors.Wrap(err, "failed to save client")
}

// GetClient gets a client by ID
func (s *Storage) GetClient(id string) (osin.Client, error) {
	conn := s.pool.Get()
	defer conn.Close()

	clientGob, err := redis.Bytes(conn.Do("GET", s.makeKey("client", id)))
	if err != nil {
		return nil, errors.Wrap(err, "failed to get client gob")
	}

	var client osin.DefaultClient
	err = decode(clientGob, &client)
	return &client, errors.Wrap(err, "failed to decode client gob")
}

// UpdateClient updates a client
func (s *Storage) UpdateClient(client osin.Client) error {
	return errors.Wrap(s.CreateClient(client), "failed to update client")
}

// DeleteClient deletes given client
func (s *Storage) DeleteClient(client osin.Client) error {
	conn := s.pool.Get()
	defer conn.Close()

	_, err := conn.Do("DEL", s.makeKey("client", client.GetId()))
	return errors.Wrap(err, "failed to delete client")
}

// SaveAuthorize saves authorize data.
func (s *Storage) SaveAuthorize(data *osin.AuthorizeData) (err error) {
	conn := s.pool.Get()
	defer conn.Close()

	payload, err := encode(data)
	if err != nil {
		return errors.Wrap(err, "failed to encode data")
	}

	_, err = conn.Do("SETEX", s.makeKey("auth", data.Code), data.ExpiresIn, string(payload))
	return errors.Wrap(err, "failed to set auth")
}

// LoadAuthorize looks up AuthorizeData by a code.
// Client information MUST be loaded together.
// Optionally can return error if expired.
func (s *Storage) LoadAuthorize(code string) (*osin.AuthorizeData, error) {
	conn := s.pool.Get()
	defer conn.Close()

	var (
		rawAuthGob interface{}
		err        error
	)

	if rawAuthGob, err = conn.Do("GET", s.makeKey("auth", code)); err != nil {
		return nil, errors.Wrap(err, "unable to GET auth")
	}
	if rawAuthGob == nil {
		return nil, errors.New("token is expired")
	}

	authGob, _ := redis.Bytes(rawAuthGob, err)

	var auth osin.AuthorizeData
	err = decode(authGob, &auth)
	return &auth, errors.Wrap(err, "failed to decode auth")
}

// RemoveAuthorize revokes or deletes the authorization code.
func (s *Storage) RemoveAuthorize(code string) (err error) {
	conn := s.pool.Get()
	defer conn.Close()

	_, err = conn.Do("DEL", s.makeKey("auth", code))
	return errors.Wrap(err, "failed to delete auth")
}

// SaveAccess creates AccessData.
func (s *Storage) SaveAccess(data *osin.AccessData) (err error) {
	conn := s.pool.Get()
	defer conn.Close()

	payload, err := encode(data)
	if err != nil {
		return errors.Wrap(err, "failed to encode access")
	}

	accessID := uuid.NewV4().String()

	if _, err := conn.Do("SET", s.makeKey("access", accessID), string(payload)); err != nil {
		return errors.Wrap(err, "failed to save access")
	}

	if _, err := conn.Do("SET", s.makeKey("access_token", data.AccessToken), accessID); err != nil {
		return errors.Wrap(err, "failed to register access token")
	}

	_, err = conn.Do("SET", s.makeKey("refresh_token", data.RefreshToken), accessID)
	return errors.Wrap(err, "failed to register refresh token")
}

// LoadAccess gets access data with given access token
func (s *Storage) LoadAccess(token string) (*osin.AccessData, error) {
	return s.loadAndRefreshAccess(s.makeKey("access_token", token))
}

// RemoveAccess deletes AccessData with given access token
func (s *Storage) RemoveAccess(token string) error {
	return s.removeAccessImpl(s.makeKey("access_token", token))
}

// LoadRefresh gets access data with given refresh token
func (s *Storage) LoadRefresh(token string) (*osin.AccessData, error) {
	return s.loadAndRefreshAccess(s.makeKey("refresh_token", token))
}

// RemoveRefresh deletes AccessData with given refresh token
func (s *Storage) RemoveRefresh(token string) error {
	return s.removeAccessImpl(s.makeKey("refresh_token", token))
}

func (s *Storage) removeAccessImpl(key string) error {
	conn := s.pool.Get()
	defer conn.Close()

	accessID, err := redis.String(conn.Do("GET", key))
	if err != nil {
		return errors.Wrapf(err, "failed to get access for %s", key)
	}

	access, err := s.loadAccessImpl(conn, key)
	if err != nil {
		return errors.Wrap(err, "unable to load access for removal")
	}

	accessKey := s.makeKey("access", accessID)
	if _, err := conn.Do("DEL", accessKey); err != nil {
		return errors.Wrapf(err, "failed to delete access for %s", accessKey)
	}

	accessTokenKey := s.makeKey("access_token", access.AccessToken)
	if _, err := conn.Do("DEL", accessTokenKey); err != nil {
		return errors.Wrapf(err, "failed to deregister access_token for %s", accessTokenKey)
	}

	refreshTokenKey := s.makeKey("refresh_token", access.RefreshToken)
	_, err = conn.Do("DEL", refreshTokenKey)
	return errors.Wrapf(err, "failed to deregister refresh_token for %s", refreshTokenKey)
}

func (s *Storage) loadAndRefreshAccess(key string) (*osin.AccessData, error) {
	conn := s.pool.Get()
	defer conn.Close()

	access, err := s.loadAccessImpl(conn, key)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to load access for %s", key)
	}

	return s.refreshAccessClients(conn, access)
}

// LoadAccess gets access data with given access token
func (s *Storage) loadAccessImpl(conn redis.Conn, key string) (*osin.AccessData, error) {
	accessID, err := redis.String(conn.Do("GET", key))
	if err != nil {
		return nil, errors.Wrapf(err, "unable to get access ID for key %s", key)
	}

	accessIDKey := s.makeKey("access", accessID)
	accessGob, err := redis.Bytes(conn.Do("GET", accessIDKey))
	if err != nil {
		return nil, errors.Wrapf(err, "unable to get access gob for key %s", accessIDKey)
	}

	var access osin.AccessData
	err = decode(accessGob, &access)
	return &access, errors.Wrap(err, "failed to decode access gob")
}

func (s *Storage) refreshAccessClients(conn redis.Conn, access *osin.AccessData) (*osin.AccessData, error) {
	var err error
	access.Client, err = s.GetClient(access.Client.GetId())
	if err != nil {
		return nil, errors.Wrap(err, "unable to get client for access")
	}

	if access.AuthorizeData != nil && access.AuthorizeData.Client != nil {
		access.AuthorizeData.Client, err = s.GetClient(access.AuthorizeData.Client.GetId())
		if err != nil {
			return nil, errors.Wrap(err, "unable to get client for access authorize data")
		}
	}

	return access, nil
}

func (s *Storage) makeKey(namespace, id string) string {
	return fmt.Sprintf("%s:%s:%s", s.keyPrefix, namespace, id)
}

func encode(v interface{}) ([]byte, error) {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(v); err != nil {
		return nil, errors.Wrap(err, "unable to encode")
	}
	return buf.Bytes(), nil
}

func decode(data []byte, v interface{}) error {
	err := gob.NewDecoder(bytes.NewBuffer(data)).Decode(v)
	return errors.Wrap(err, "unable to decode")
}