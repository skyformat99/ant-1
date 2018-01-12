// Copyright 2017 HenryLee. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ants

import (
	"strings"
	"time"

	"github.com/henrylee2cn/cfgo"
	"github.com/henrylee2cn/goutil"
	tp "github.com/henrylee2cn/teleport"
	"github.com/henrylee2cn/teleport/socket"
	heartbeat "github.com/henrylee2cn/tp-ext/plugin-heartbeat"
	cliSession "github.com/henrylee2cn/tp-ext/sundry-cliSession"
)

// CliConfig client config
// Note:
//  yaml tag is used for github.com/henrylee2cn/cfgo
//  ini tag is used for github.com/henrylee2cn/ini
type CliConfig struct {
	TlsCertFile         string        `yaml:"tls_cert_file"          ini:"tls_cert_file"          comment:"TLS certificate file path"`
	TlsKeyFile          string        `yaml:"tls_key_file"           ini:"tls_key_file"           comment:"TLS key file path"`
	DefaultReadTimeout  time.Duration `yaml:"default_read_timeout"   ini:"default_read_timeout"   comment:"Default maximum duration for reading; ns,µs,ms,s,m,h"`
	DefaultWriteTimeout time.Duration `yaml:"default_write_timeout"  ini:"default_write_timeout"  comment:"Default maximum duration for writing; ns,µs,ms,s,m,h"`
	DefaultDialTimeout  time.Duration `yaml:"default_dial_timeout"   ini:"default_dial_timeout"   comment:"Default maximum duration for dialing; for client role; ns,µs,ms,s,m,h"`
	RedialTimes         int32         `yaml:"redial_times"           ini:"redial_times"           comment:"The maximum times of attempts to redial, after the connection has been unexpectedly broken; for client role"`
	SlowCometDuration   time.Duration `yaml:"slow_comet_duration"    ini:"slow_comet_duration"    comment:"Slow operation alarm threshold; ns,µs,ms,s ..."`
	DefaultBodyCodec    string        `yaml:"default_body_codec"     ini:"default_body_codec"     comment:"Default body codec type id"`
	PrintBody           bool          `yaml:"print_body"             ini:"print_body"             comment:"Is print body or not"`
	CountTime           bool          `yaml:"count_time"             ini:"count_time"             comment:"Is count cost time or not"`
	Network             string        `yaml:"network"                ini:"network"                comment:"Network; tcp, tcp4, tcp6, unix or unixpacket"`
	Heartbeat           time.Duration `yaml:"heartbeat"              ini:"heartbeat"              comment:"When the heartbeat interval is greater than 0, heartbeat is enabled; ns,µs,ms,s,m,h"`
	SessMaxQuota        int           `yaml:"sess_max_quota"         ini:"sess_max_quota"         comment:"The maximum number of sessions in the connection pool"`
	SessMaxIdleDuration time.Duration `yaml:"sess_max_idle_duration" ini:"sess_max_idle_duration" comment:"The maximum time period for the idle session in the connection pool; ns,µs,ms,s,m,h"`
}

// Reload Bi-directionally synchronizes config between YAML file and memory.
func (c *CliConfig) Reload(bind cfgo.BindFunc) error {
	err := bind()
	if err != nil {
		return err
	}
	return c.check()
}
func (c *CliConfig) check() error {
	if c.SessMaxQuota <= 0 {
		c.SessMaxQuota = 100
	}
	if c.SessMaxIdleDuration <= 0 {
		c.SessMaxIdleDuration = time.Minute * 3
	}
	return nil
}

func (c *CliConfig) peerConfig() tp.PeerConfig {
	return tp.PeerConfig{
		DefaultReadTimeout:  c.DefaultReadTimeout,
		DefaultWriteTimeout: c.DefaultWriteTimeout,
		DefaultDialTimeout:  c.DefaultDialTimeout,
		RedialTimes:         c.RedialTimes,
		SlowCometDuration:   c.SlowCometDuration,
		DefaultBodyCodec:    c.DefaultBodyCodec,
		PrintBody:           c.PrintBody,
		CountTime:           c.CountTime,
		Network:             c.Network,
	}
}

// Client client peer
type Client struct {
	peer                *tp.Peer
	linker              Linker
	protoFunc           socket.ProtoFunc
	cliSessPool         goutil.Map
	sessMaxQuota        int
	sessMaxIdleDuration time.Duration
}

// NewClient creates a client peer.
func NewClient(cfg CliConfig, plugin ...tp.Plugin) *Client {
	if err := cfg.check(); err != nil {
		tp.Fatalf("%v", err)
	}
	if cfg.Heartbeat > 0 {
		plugin = append(plugin, heartbeat.NewPing(cfg.Heartbeat))
	}
	peer := tp.NewPeer(cfg.peerConfig(), plugin...)
	if len(cfg.TlsCertFile) > 0 && len(cfg.TlsKeyFile) > 0 {
		err := peer.SetTlsConfigFromFile(cfg.TlsCertFile, cfg.TlsKeyFile)
		if err != nil {
			tp.Fatalf("%v", err)
		}
	}
	return &Client{
		peer:                peer,
		protoFunc:           socket.DefaultProtoFunc(),
		cliSessPool:         goutil.AtomicMap(),
		sessMaxQuota:        cfg.SessMaxQuota,
		sessMaxIdleDuration: cfg.SessMaxIdleDuration,
	}
}

// SetProtoFunc sets socket.ProtoFunc.
func (c *Client) SetProtoFunc(protoFunc socket.ProtoFunc) {
	c.protoFunc = protoFunc
}

// SetLinker sets Linker.
func (c *Client) SetLinker(linker Linker) {
	c.linker = linker
}

// AsyncPull sends a packet and receives reply asynchronously.
// If the args is []byte or *[]byte type, it can automatically fill in the body codec name.
func (c *Client) AsyncPull(uri string, args interface{}, reply interface{}, done chan tp.PullCmd, setting ...socket.PacketSetting) {
	cliSess, rerr := c.getCliSession(uri)
	if rerr != nil {
		done <- cliSession.NewFakePullCmd(c.peer, uri, args, reply, rerr, setting...)
		return
	}
	cliSess.AsyncPull(uri, args, reply, done, setting...)
}

// Pull sends a packet and receives reply.
// Note:
// If the args is []byte or *[]byte type, it can automatically fill in the body codec name;
// If the session is a client role and PeerConfig.RedialTimes>0, it is automatically re-called once after a failure.
func (c *Client) Pull(uri string, args interface{}, reply interface{}, setting ...socket.PacketSetting) tp.PullCmd {
	cliSess, rerr := c.getCliSession(uri)
	if rerr != nil {
		return cliSession.NewFakePullCmd(c.peer, uri, args, reply, rerr, setting...)
	}
	return cliSess.Pull(uri, args, reply, setting...)
}

// Push sends a packet, but do not receives reply.
// Note:
// If the args is []byte or *[]byte type, it can automatically fill in the body codec name;
// If the session is a client role and PeerConfig.RedialTimes>0, it is automatically re-called once after a failure.
func (c *Client) Push(uri string, args interface{}, setting ...socket.PacketSetting) *tp.Rerror {
	cliSess, rerr := c.getCliSession(uri)
	if rerr != nil {
		return rerr
	}
	return cliSess.Push(uri, args, setting...)
}

func (c *Client) getCliSession(uri string) (*cliSession.CliSession, *tp.Rerror) {
	if idx := strings.Index(uri, "?"); idx != -1 {
		uri = uri[:idx]
	}
	addr, rerr := c.linker.Select(uri)
	if rerr != nil {
		return nil, rerr
	}
	_cliSess, ok := c.cliSessPool.Load(addr)
	if ok {
		return _cliSess.(*cliSession.CliSession), nil
	}
	cliSess := cliSession.New(
		c.peer,
		addr,
		c.sessMaxQuota,
		c.sessMaxIdleDuration,
	)
	c.cliSessPool.Store(addr, cliSess)
	return cliSess, nil
}