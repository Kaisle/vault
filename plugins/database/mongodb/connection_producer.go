package mongodb

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/errwrap"
	"github.com/hashicorp/vault/plugins/helper/database/connutil"
	"github.com/hashicorp/vault/plugins/helper/database/dbutil"
	"github.com/mitchellh/mapstructure"
	"gopkg.in/mgo.v2"
)

// mongoDBConnectionProducer implements ConnectionProducer and provides an
// interface for databases to make connections.
type mongoDBConnectionProducer struct {
	ConnectionURL string `json:"connection_url" structs:"connection_url" mapstructure:"connection_url"`
	WriteConcern  string `json:"write_concern" structs:"write_concern" mapstructure:"write_concern"`
	Username      string `json:"username" structs:"username" mapstructure:"username"`
	Password      string `json:"password" structs:"password" mapstructure:"password"`
	TLSCert       string `json:"tls_cert" structs:"tls_cert" mapstructure:"tls_cert"`
	TLSKey        string `json:"tls_key" structs:"tls_key" mapstructure:"tls_key"`
	TLSCA         string `json:"tls_ca" structs:"tls_ca" mapstructure:"tls_ca"`
	TLSVerify     string `json:"tls_verify" structs:"tls_verify" mapstructure:"tls_verify"`

	Initialized bool
	RawConfig   map[string]interface{}
	Type        string
	session     *mgo.Session
	safe        *mgo.Safe
	sync.Mutex
}

func (c *mongoDBConnectionProducer) Initialize(ctx context.Context, conf map[string]interface{}, verifyConnection bool) error {
	_, err := c.Init(ctx, conf, verifyConnection)
	return err
}

// Initialize parses connection configuration.
func (c *mongoDBConnectionProducer) Init(ctx context.Context, conf map[string]interface{}, verifyConnection bool) (map[string]interface{}, error) {
	c.Lock()
	defer c.Unlock()

	c.RawConfig = conf

	err := mapstructure.WeakDecode(conf, c)
	if err != nil {
		return nil, err
	}

	if len(c.ConnectionURL) == 0 {
		return nil, fmt.Errorf("connection_url cannot be empty")
	}

	c.ConnectionURL = dbutil.QueryHelper(c.ConnectionURL, map[string]string{
		"username": c.Username,
		"password": c.Password,
	})

	if c.WriteConcern != "" {
		input := c.WriteConcern

		// Try to base64 decode the input. If successful, consider the decoded
		// value as input.
		inputBytes, err := base64.StdEncoding.DecodeString(input)
		if err == nil {
			input = string(inputBytes)
		}

		concern := &mgo.Safe{}
		err = json.Unmarshal([]byte(input), concern)
		if err != nil {
			return nil, errwrap.Wrapf("error mashalling write_concern: {{err}}", err)
		}

		// Guard against empty, non-nil mgo.Safe object; we don't want to pass that
		// into mgo.SetSafe in Connection().
		if (mgo.Safe{} == *concern) {
			return nil, fmt.Errorf("provided write_concern values did not map to any mgo.Safe fields")
		}
		c.safe = concern
	}

	// Set initialized to true at this point since all fields are set,
	// and the connection can be established at a later time.
	c.Initialized = true

	if verifyConnection {
		if _, err := c.Connection(ctx); err != nil {
			return nil, errwrap.Wrapf("error verifying connection: {{err}}", err)
		}

		if err := c.session.Ping(); err != nil {
			return nil, errwrap.Wrapf("error verifying connection: {{err}}", err)
		}
	}

	return conf, nil
}

// Connection creates or returns an existing a database connection. If the session fails
// on a ping check, the session will be closed and then re-created.
func (c *mongoDBConnectionProducer) Connection(_ context.Context) (interface{}, error) {
	if !c.Initialized {
		return nil, connutil.ErrNotInitialized
	}

	if c.session != nil {
		if err := c.session.Ping(); err == nil {
			return c.session, nil
		}
		c.session.Close()
	}

	dialInfo, err := parseMongoURL(c.ConnectionURL, c.TLSCert, c.TLSKey, c.TLSCA, c.TLSVerify)
	if err != nil {
		return nil, err
	}

	if err != nil {
		return nil, err
	}

	if c.safe != nil {
		c.session.SetSafe(c.safe)
	}

	c.session.SetSyncTimeout(1 * time.Minute)
	c.session.SetSocketTimeout(1 * time.Minute)

	return c.session, nil
}

// Close terminates the database connection.
func (c *mongoDBConnectionProducer) Close() error {
	c.Lock()
	defer c.Unlock()

	if c.session != nil {
		c.session.Close()
	}

	c.session = nil

	return nil
}

func parseMongoURL(rawURL, tlsCert, tlsKey, tlsCA, tlsVerify string) (*mgo.DialInfo, error) {
	url, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}

	info := mgo.DialInfo{
		Addrs:    strings.Split(url.Host, ","),
		Database: strings.TrimPrefix(url.Path, "/"),
		Timeout:  10 * time.Second,
	}

	if url.User != nil {
		info.Username = url.User.Username()
		info.Password, _ = url.User.Password()
	}

	query := url.Query()
	for key, values := range query {
		var value string
		if len(values) > 0 {
			value = values[0]
		}

		switch key {
		case "authSource":
			info.Source = value
		case "authMechanism":
			info.Mechanism = value
		case "gssapiServiceName":
			info.Service = value
		case "replicaSet":
			info.ReplicaSetName = value
		case "maxPoolSize":
			poolLimit, err := strconv.Atoi(value)
			if err != nil {
				return nil, errors.New("bad value for maxPoolSize: " + value)
			}
			info.PoolLimit = poolLimit
		case "ssl":
			// Unfortunately, mgo doesn't support the ssl parameter in its MongoDB URI parsing logic, so we have to handle that
			// ourselves. See https://github.com/go-mgo/mgo/issues/84
			ssl, err := strconv.ParseBool(value)
			if err != nil {
				return nil, errors.New("bad value for ssl: " + value)
			}
			if ssl {
				info.DialServer = func(addr *mgo.ServerAddr) (net.Conn, error) {
					tlsConfig := &tls.Config{}
					if tlsCert != "" && tlsKey != "" && tlsCA != "" {
						caCerts := x509.NewCertPool()
						ok := caCerts.AppendCertsFromPEM([]byte(tlsCA))
						if !ok {
							return nil, errors.New("failed to parse tls_ca value")
						}
						clientCert, err := tls.X509KeyPair([]byte(tlsCert), []byte(tlsKey))
						if err != nil {
							return nil, errors.New("bad value for tls_cert or tls_key")
						}
						clientCert.Leaf, err = x509.ParseCertificate(clientCert.Certificate[0])
						if err != nil {
							return nil, errors.New("failed to parse tls_cert or tls_key")
						}
						insecureSkipVerify := false
						if tlsVerify != "" {
							insecureSkipVerify, err = strconv.ParseBool(tlsVerify)
							if err != nil {
								return nil, errors.New("bad value for tls verify: " + tlsVerify)
							}
						}
						tlsConfig = &tls.Config{
							Certificates:       []tls.Certificate{clientCert},
							RootCAs:            caCerts,
							InsecureSkipVerify: insecureSkipVerify,
						}
					}
					return tls.Dial("tcp", addr.String(), tlsConfig)
				}
			}
		case "connect":
			if value == "direct" {
				info.Direct = true
				break
			}
			if value == "replicaSet" {
				break
			}
			fallthrough
		default:
			return nil, errors.New("unsupported connection URL option: " + key + "=" + value)
		}
	}

	return &info, nil
}

func (c *mongoDBConnectionProducer) secretValues() map[string]interface{} {
	return map[string]interface{}{
		c.Password: "[password]",
	}
}
