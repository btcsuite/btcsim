package main

import (
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	rpc "github.com/conformal/btcrpcclient"
	"github.com/conformal/btcutil"
	"github.com/conformal/btcwire"
)

// BtcdArgs contains all the args and data required to launch a btcd
// instance and connect the rpc client to it
type BtcdArgs struct {
	RPCUser    string
	RPCPass    string
	Listen     string
	RPCListen  string
	RPCConnect string
	RPCCert    string
	RPCKey     string
	DataDir    string
	LogDir     string
	Profile    string
	DebugLevel string
	Extra      []string

	prefix       string
	exe          string
	endpoint     string
	certificates []byte
}

// NewBtcdArgs returns a BtcdArgs with all default values
func NewBtcdArgs(prefix string) (*BtcdArgs, error) {
	a := &BtcdArgs{
		Listen:    "127.0.0.1:18555",
		RPCListen: "127.0.0.1:18556",
		RPCUser:   "user",
		RPCPass:   "pass",

		prefix:   prefix,
		exe:      "btcd",
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
func (a *BtcdArgs) SetDefaults() error {
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
	appDir := btcutil.AppDataDir(a.exe, false)
	a.RPCCert = filepath.Join(appDir, "rpc.cert")
	a.RPCKey = filepath.Join(appDir, "rpc.key")
	cert, err := ioutil.ReadFile(a.RPCCert)
	if err != nil {
		return err
	}
	a.certificates = cert
	return nil
}

// String returns a printable name of this instance
func (a *BtcdArgs) String() string {
	return a.prefix
}

// Arguments returns an array of arguments that be used to launch the
// btcd instance
func (a *BtcdArgs) Arguments() []string {
	args := []string{}
	// --simnet
	args = append(args, fmt.Sprintf("--%s", strings.ToLower(btcwire.SimNet.String())))
	if a.RPCUser != "" {
		// --rpcuser
		args = append(args, fmt.Sprintf("--rpcuser=%s", a.RPCUser))
	}
	if a.RPCPass != "" {
		// --rpcpass
		args = append(args, fmt.Sprintf("--rpcpass=%s", a.RPCPass))
	}
	if a.Listen != "" {
		// --listen
		args = append(args, fmt.Sprintf("--listen=%s", a.Listen))
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

// Command returns Cmd of the btcd instance
func (a *BtcdArgs) Command() *exec.Cmd {
	return exec.Command(a.exe, a.Arguments()...)
}

// RPCConnConfig returns the rpc connection config that can be used
// to connect to the btcd instance that is launched on Start
func (a *BtcdArgs) RPCConnConfig() rpc.ConnConfig {
	return rpc.ConnConfig{
		Host:                 a.RPCListen,
		Endpoint:             a.endpoint,
		User:                 a.RPCUser,
		Pass:                 a.RPCPass,
		Certificates:         a.certificates,
		DisableAutoReconnect: true,
	}
}

// Cleanup removes the tmp data and log directories
func (a *BtcdArgs) Cleanup() error {
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
