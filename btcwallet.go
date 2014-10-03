package main

import (
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"strings"

	rpc "github.com/conformal/btcrpcclient"
	"github.com/conformal/btcwire"
)

// BtcwalletArgs contains all the args and data required to launch a btcwallet
// instance and connect the rpc client to it
type BtcwalletArgs struct {
	Username   string
	Password   string
	RPCListen  string
	RPCConnect string
	RPCCert    string
	RPCKey     string
	DataDir    string
	LogDir     string
	Profile    string
	DebugLevel string

	Extra        []string
	Certificates []byte

	prefix   string
	exe      string
	endpoint string
}

// NewBtcwalletArgs returns a BtcwalletArgs with all default values
func NewBtcwalletArgs(port uint16, nodeArgs *BtcdArgs) (*BtcwalletArgs, error) {
	a := &BtcwalletArgs{
		RPCListen:    fmt.Sprintf("127.0.0.1:%d", port),
		RPCConnect:   "127.0.0.1:18556",
		Username:     "user",
		Password:     "pass",
		Certificates: nodeArgs.certificates,
		RPCCert:      nodeArgs.RPCCert,
		RPCKey:       nodeArgs.RPCKey,

		prefix:   fmt.Sprintf("actor-%d", port),
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
	datadir, err := ioutil.TempDir("", a.prefix+"-data")
	if err != nil {
		return err
	}
	a.DataDir = datadir
	logdir, err := ioutil.TempDir("", a.prefix+"-logs")
	if err != nil {
		return err
	}
	a.LogDir = logdir
	return nil
}

// String returns a printable name of this instance
func (a *BtcwalletArgs) String() string {
	return a.prefix
}

// Arguments returns an array of arguments that be used to launch the
// btcwallet instance
func (a *BtcwalletArgs) Arguments() []string {
	args := []string{}
	// --simnet
	args = append(args, fmt.Sprintf("--%s", strings.ToLower(btcwire.SimNet.String())))
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
	if a.RPCCert != "" {
		// --rpccert
		args = append(args, fmt.Sprintf("--rpccert=%s", a.RPCCert))
	}
	if a.RPCKey != "" {
		// --rpckey
		args = append(args, fmt.Sprintf("--rpckey=%s", a.RPCKey))
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
