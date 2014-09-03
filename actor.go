// Copyright (c) 2014 Conformal Systems LLC.
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package main

import (
	"errors"
	"io"
	"io/ioutil"
	"log"
	"math/rand"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/conformal/btcchain"
	"github.com/conformal/btcjson"
	rpc "github.com/conformal/btcrpcclient"
	"github.com/conformal/btcutil"
	"github.com/conformal/btcwire"
)

// minFee is the minimum tx fee that can be paid
const minFee btcutil.Amount = 1e4 // 0.0001 BTC

// Actor describes an actor on the simulation network.  Each actor runs
// independantly without external input to decide it's behavior.
type Actor struct {
	args         procArgs
	cmd          *exec.Cmd
	client       *rpc.Client
	maxAddresses int
	addrs        map[string]struct{}
	downstream   chan btcutil.Address
	upstream     chan btcutil.Address
	txpool       chan struct{}
	errChan      chan struct{}
	quit         chan struct{}
	utxo         chan *TxOut
	coinbase     chan *btcutil.Tx
	wg           sync.WaitGroup
	closed       bool
}

// TxOut is a valid tx output that can be used to generate transactions
type TxOut struct {
	OutPoint *btcwire.OutPoint
	Amount   btcutil.Amount
}

// NewActor creates a new actor which runs its own wallet process connecting
// to the btcd chain server specified by chain, and listening for simulator
// websocket connections on the specified port.
func NewActor(chain *ChainServer, port uint16) (*Actor, error) {
	// Please don't run this as root.
	if port < 1024 {
		return nil, errors.New("invalid actor port")
	}

	dir, err := ioutil.TempDir("", "actor")
	if err != nil {
		return nil, err
	}

	a := Actor{
		args: procArgs{
			chainSvr:         *chain,
			dir:              dir,
			port:             strconv.FormatUint(uint64(port), 10),
			walletPassphrase: "walletpass",
		},
		coinbase:     make(chan *btcutil.Tx, btcchain.CoinbaseMaturity),
		addrs:        make(map[string]struct{}),
		maxAddresses: *maxAddresses,
		quit:         make(chan struct{}),
		utxo:         make(chan *TxOut),
	}
	return &a, nil
}

// Start creates the command to execute a wallet process and starts the
// command in the background, attaching the command's stderr and stdout
// to the passed writers. Nil writers may be used to discard output.
//
// In addition to starting the wallet process, this runs goroutines to
// handle wallet notifications and requests the wallet process to create
// an intial encrypted wallet, so that it can actually send and receive BTC.
//
// If the RPC client connection cannot be established or wallet cannot
// be created, the wallet process is killed and the actor directory
// removed.
func (a *Actor) Start(stderr, stdout io.Writer, com *Communication) error {
	a.downstream = com.downstream
	a.upstream = com.upstream
	a.errChan = com.errChan
	a.txpool = com.txpool

	// Overwriting the previously created command would be sad.
	if a.cmd != nil {
		a.errChan <- struct{}{}
		return errors.New("actor command previously created")
	}

	connected := make(chan struct{})
	var firstConn bool
	const timeoutSecs int64 = 3600 * 24

	// Create and start command in background.
	a.cmd = a.args.Cmd()
	a.cmd.Stdout = stdout
	a.cmd.Stderr = stderr
	if err := a.cmd.Start(); err != nil {
		if err := a.Cleanup(); err != nil {
			log.Printf("Cannot remove actor directory after "+
				"failed start of wallet process: %v", err)
		}
		a.errChan <- struct{}{}
		return err
	}

	// Create and start RPC client.
	rpcConf := rpc.ConnConfig{
		Host:                 "localhost:" + a.args.port,
		Endpoint:             "ws",
		User:                 a.args.chainSvr.user,
		Pass:                 a.args.chainSvr.pass,
		Certificates:         a.args.chainSvr.cert,
		DisableAutoReconnect: true,
	}

	ntfnHandlers := rpc.NotificationHandlers{
		OnBtcdConnected: func(conn bool) {
			if conn && !firstConn {
				firstConn = true
				connected <- struct{}{}
			}
		},
	}

	// The RPC client will not wait for the RPC server to start up, so
	// loop a few times and attempt additional connections, sleeping
	// after each failure.
	var client *rpc.Client
	var connErr error
	for i := 0; i < *maxConnRetries; i++ {
		if client, connErr = rpc.New(&rpcConf, &ntfnHandlers); connErr != nil {
			time.Sleep(time.Duration(i) * 50 * time.Millisecond)
			continue
		}
		a.client = client
		break
	}
	if a.client == nil {
		a.errChan <- struct{}{}
		return connErr
	}

	// Wait for btcd to connect
	<-connected

	// Create the wallet.
	if err := a.client.CreateEncryptedWallet(a.args.walletPassphrase); err != nil {
		a.errChan <- struct{}{}
		return err
	}

	// Wait for wallet sync
	for i := 0; i < *maxConnRetries; i++ {
		if _, err := a.client.GetBalance(""); err != nil {
			time.Sleep(time.Duration(i) * 50 * time.Millisecond)
			continue
		}
		break
	}

	if err := a.client.WalletPassphrase(a.args.walletPassphrase, timeoutSecs); err != nil {
		log.Printf("%s: Cannot unlock wallet: %v", rpcConf.Host, err)
		a.errChan <- struct{}{}
		return err
	}

	// Send a random address upstream that will be used by the cpu miner.
	addr, err := a.client.GetNewAddress()
	if err != nil {
		log.Printf("%s: Cannot create mining address", rpcConf.Host)
		a.errChan <- struct{}{}
		return err
	}
	a.addrs[addr.String()] = struct{}{}
	a.upstream <- addr

	// Start a goroutine to send addresses upstream.
	a.wg.Add(1)
	go a.SendAddrs()

	// Start a goroutine to send transactions.
	a.wg.Add(1)
	go a.SendTx()

	return nil
}

// SendAddrs generates addrs and sends them upstream
// the first a.maxAddresses addrs are generated on the fly
// after that, addrs are sent at random
func (a *Actor) SendAddrs() {
	defer a.wg.Done()

	var i int
	var addr btcutil.Address
	var err error
	addressSpace := make([]btcutil.Address, a.maxAddresses)
	for {
		// if maxAddresses have not been generated yet, generate
		// and keep an addr ready
		if i < a.maxAddresses {
			addr, err = a.client.GetNewAddress()
			if err != nil {
				log.Printf("Cannot create address #%d", i+1)
				a.errChan <- struct{}{}
				return
			}
			a.addrs[addr.String()] = struct{}{}
			addressSpace[i] = addr
			i++
		} else {
			// if we already have maxAddresses, just return a random addr
			addr = addressSpace[rand.Int()%a.maxAddresses]
		}
		select {
		case a.upstream <- addr:
			// Send address to upstream to request receiving a transaction.
		case <-a.quit:
			return
		}
	}
}

// SendTx creates and sends tx
func (a *Actor) SendTx() {
	defer a.wg.Done()

	for {
		select {
		case utxo := <-a.utxo:
			select {
			case addr := <-a.downstream:
				// Create a raw transaction
				txid := utxo.OutPoint.Hash.String()
				inputs := []btcjson.TransactionInput{{txid, utxo.OutPoint.Index}}
				amounts := map[btcutil.Address]btcutil.Amount{addr: utxo.Amount - minFee}
				msgTx, err := a.client.CreateRawTransaction(inputs, amounts)
				if err != nil {
					log.Printf("Cannot create raw transaction: %v", err)
					if a.txpool != nil {
						a.txpool <- struct{}{}
					}
					continue
				}
				// sign it
				msgTx, ok, err := a.client.SignRawTransaction(msgTx)
				if err != nil {
					log.Printf("Cannot sign raw transaction: %v", err)
					if a.txpool != nil {
						a.txpool <- struct{}{}
					}
					continue
				}
				if !ok {
					log.Printf("Not all inputs have been signed: %v", err)
					if a.txpool != nil {
						a.txpool <- struct{}{}
					}
					continue
				}
				// and finally send it.
				if _, err := a.client.SendRawTransaction(msgTx, false); err != nil {
					log.Printf("Cannot send raw transaction: %v", err)
					if a.txpool != nil {
						a.txpool <- struct{}{}
					}
					continue
				}
			case <-a.quit:
				return
			}
		case <-a.quit:
			return
		}
	}
}

// Stop closes the client and quit channel so every running goroutine
// can return or just exits if quit has already been closed.
func (a *Actor) Stop() {
	select {
	case <-a.quit:
	default:
		go a.drainChans()
		a.client.Shutdown()
		close(a.quit)
	}
}

// WaitForShutdown waits until every actor goroutine has returned
func (a *Actor) WaitForShutdown() {
	a.wg.Wait()
}

// Shutdown kills the actor btcwallet process and removes its data directories.
func (a *Actor) Shutdown() {
	if !a.closed {
		a.client.Shutdown()
		if err := Exit(a.cmd); err != nil {
			log.Printf("Cannot exit actor on %s: %v", "localhost:"+a.args.port, err)
		}
		if err := a.Cleanup(); err != nil {
			log.Printf("Cannot cleanup actor directory on %s: %v", "localhost:"+a.args.port, err)
		}
		a.closed = true
		log.Printf("Actor on %s shutdown successfully", "localhost:"+a.args.port)
	} else {
		log.Printf("Actor on %s already shutdown", "localhost:"+a.args.port)
	}
}

// drainChans drains all chans that might block an actor from shutting down
func (a *Actor) drainChans() {
	for {
		select {
		case <-a.downstream:
		case <-a.upstream:
		case <-a.txpool:
		case <-a.utxo:
		case <-a.errChan:
		}
	}
}

// Cleanup removes the directory an Actor's wallet process was previously using.
func (a *Actor) Cleanup() error {
	return os.RemoveAll(a.args.dir)
}

// ForceShutdown shutdowns an actor that unexpectedly exited a.Start
func (a *Actor) ForceShutdown() {
	a.Stop()
	a.WaitForShutdown()
	a.Shutdown()
}

type procArgs struct {
	chainSvr         ChainServer
	dir              string
	port             string
	walletPassphrase string
}

func (p *procArgs) Cmd() *exec.Cmd {
	return exec.Command("btcwallet", p.args()...)
}

func (p *procArgs) args() []string {
	return []string{
		"-d" + "TXST=warn",
		"--simnet",
		"--datadir=" + p.dir,
		"--username=" + p.chainSvr.user,
		"--password=" + p.chainSvr.pass,
		"--rpcconnect=" + p.chainSvr.connect,
		"--rpclisten=:" + p.port,
		"--rpccert=" + p.chainSvr.certPath,
		"--rpckey=" + p.chainSvr.keyPath,
	}
}
