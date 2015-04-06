/*
 * Copyright (c) 2014-2015 Conformal Systems LLC <info@conformal.com>
 *
 * Permission to use, copy, modify, and distribute this software for any
 * purpose with or without fee is hereby granted, provided that the above
 * copyright notice and this permission notice appear in all copies.
 *
 * THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
 * WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
 * MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
 * ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
 * WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
 * ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
 * OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
 */

package simnode

import (
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"strings"

	"github.com/btcsuite/btcd/wire"
	rpc "github.com/btcsuite/btcrpcclient"
)

// BtcwalletArgs contains all the args and data required to launch a btcwallet
// instance and connect the rpc client to it
type BtcwalletArgs struct {
	Username   string
	Password   string
	RPCListen  string
	RPCConnect string
	CAFile     string
	KeyFile    string // TODO(roasbeef): Needs to be public??
	DataDir    string
	LogDir     string
	Profile    string
	DebugLevel string
	Prefix     string

	Extra        []string
	Certificates []byte

	exe      string
	endpoint string
	certFile string
}

// newBtcwalletArgs returns a BtcwalletArgs with all default values
func NewBtcwalletArgs(port uint16, nodeArgs *BtcdArgs) (*BtcwalletArgs, error) {
	a := &BtcwalletArgs{
		RPCListen:    fmt.Sprintf("127.0.0.1:%d", port),
		RPCConnect:   "127.0.0.1:18556",
		Username:     "user",
		Password:     "pass",
		Certificates: nodeArgs.certificates,
		CAFile:       nodeArgs.certFile,
		KeyFile:      nodeArgs.keyFile,

		Prefix:   fmt.Sprintf("actor-%d", port),
		exe:      "btcwallet",
		endpoint: "ws",
	}
	if err := a.SetDefaults(); err != nil {
		return nil, err
	}
	return a, nil
}

// SetDefaults sets the default values of args
// it creates tmp data and log directories and must
// be cleaned up by calling Cleanup
func (a *BtcwalletArgs) SetDefaults() error {
	datadir, err := ioutil.TempDir("", a.Prefix+"-data")
	if err != nil {
		return err
	}
	a.DataDir = datadir
	logdir, err := ioutil.TempDir("", a.Prefix+"-logs")
	if err != nil {
		return err
	}
	a.LogDir = logdir
	return nil
}

// String returns a printable name of this instance
func (a *BtcwalletArgs) String() string {
	return a.Prefix
}

// Arguments returns an array of arguments that be used to launch the
// btcwallet instance
func (a *BtcwalletArgs) Arguments() []string {
	args := []string{}
	// --simnet
	args = append(args, fmt.Sprintf("--%s", strings.ToLower(wire.SimNet.String())))
	if a.Username != "" {
		// --username
		args = append(args, fmt.Sprintf("--username=%s", a.Username))
	}
	if a.Password != "" {
		// --password
		args = append(args, fmt.Sprintf("--password=%s", a.Password))
	}
	if a.RPCListen != "" {
		// --rpclisten
		args = append(args, fmt.Sprintf("--rpclisten=%s", a.RPCListen))
	}
	if a.RPCConnect != "" {
		// --rpcconnect
		args = append(args, fmt.Sprintf("--rpcconnect=%s", a.RPCConnect))
	}
	// --rpccert
	args = append(args, fmt.Sprintf("--rpccert=%s", a.CAFile))
	// --rpckey
	args = append(args, fmt.Sprintf("--rpckey=%s", a.KeyFile))
	if a.CAFile != "" {
		// --cafile
		args = append(args, fmt.Sprintf("--cafile=%s", a.CAFile))
	}
	if a.DataDir != "" {
		// --datadir
		args = append(args, fmt.Sprintf("--datadir=%s", a.DataDir))
	}
	if a.LogDir != "" {
		// --logdir
		args = append(args, fmt.Sprintf("--logdir=%s", a.LogDir))
	}
	if a.Profile != "" {
		// --profile
		args = append(args, fmt.Sprintf("--profile=%s", a.Profile))
	}
	if a.DebugLevel != "" {
		// --debuglevel
		args = append(args, fmt.Sprintf("--debuglevel=%s", a.DebugLevel))
	}
	// --createtemp
	args = append(args, "--createtemp")
	args = append(args, a.Extra...)
	return args
}

// Command returns Cmd of the btcwallet instance
func (a *BtcwalletArgs) Command() *exec.Cmd {
	return exec.Command(a.exe, a.Arguments()...)
}

// RPCConnConfig returns the rpc connection config that can be used
// to connect to the btcwallet instance that is launched on Start
func (a *BtcwalletArgs) RPCConnConfig() rpc.ConnConfig {
	return rpc.ConnConfig{
		Host:                 a.RPCListen,
		Endpoint:             a.endpoint,
		User:                 a.Username,
		Pass:                 a.Password,
		Certificates:         a.Certificates,
		DisableAutoReconnect: true,
	}
}

// Cleanup removes the tmp data and log directories
func (a *BtcwalletArgs) Cleanup() error {
	dirs := []string{
		a.LogDir,
		a.DataDir,
	}
	var err error
	for _, dir := range dirs {
		if err = os.RemoveAll(dir); err != nil {
			log.Printf("Cannot remove dir %s: %v", dir, err)
		}
	}
	return err
}
