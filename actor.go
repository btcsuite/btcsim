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

	"github.com/conformal/btcjson"
	rpc "github.com/conformal/btcrpcclient"
	"github.com/conformal/btcutil"
)

// Actor describes an actor on the simulation network.  Each actor runs
// independantly without external input to decide it's behavior.
type Actor struct {
	args         procArgs
	cmd          *exec.Cmd
	client       *rpc.Client
	maxAddresses int
	downstream   chan btcutil.Address
	upstream     chan btcutil.Address
	stop         chan struct{}
	quit         chan struct{}
	wg           sync.WaitGroup
	closed       bool
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
		maxAddresses: *maxAddresses,
		quit:         make(chan struct{}),
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
func (a *Actor) Start(stderr, stdout io.Writer, com Communication) error {
	// Overwriting the previously created command would be sad.
	if a.cmd != nil {
		return errors.New("actor command previously created")
	}

	// Starting amount at 50 BTC
	amount := btcutil.Amount(50 * btcutil.SatoshiPerBitcoin)
	spendAfter := make(chan struct{})
	start := true
	connected := make(chan struct{})
	var firstConn bool
	const timeoutSecs int64 = 3600 * 24

	a.downstream = com.downstream
	a.upstream = com.upstream
	a.stop = com.stop

	// Create and start command in background.
	a.cmd = a.args.Cmd()
	a.cmd.Stdout = stdout
	a.cmd.Stderr = stderr
	if err := a.cmd.Start(); err != nil {
		if err := a.Cleanup(); err != nil {
			log.Printf("Cannot remove actor directory after "+
				"failed start of wallet process: %v", err)
		}
		return err
	}

	// Create and start RPC client.
	rpcConf := rpc.ConnConfig{
		Host:         "localhost:" + a.args.port,
		Endpoint:     "ws",
		User:         a.args.chainSvr.user,
		Pass:         a.args.chainSvr.pass,
		Certificates: a.args.chainSvr.cert,
	}

	ntfnHandlers := rpc.NotificationHandlers{
		OnBtcdConnected: func(conn bool) {
			if conn && !firstConn {
				firstConn = true
				connected <- struct{}{}
			}
		},
		OnAccountBalance: func(account string, balance btcutil.Amount, confirmed bool) {
			// Block actors at start until coinbase matures (equals or is more than 50 BTC).
			if balance >= amount && start {
				spendAfter <- struct{}{}
				start = false
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
		a.Shutdown()
		return connErr
	}

	// Wait for btcd to connect
	<-connected

	// Create the wallet.
	if err := a.client.CreateEncryptedWallet(a.args.walletPassphrase); err != nil {
		a.Shutdown()
		return err
	}

	// Create wallet addresses and unlock wallet.
	log.Printf("%s: Creating wallet addresses. This may take a while...", rpcConf.Host)
	addressSpace := make([]btcutil.Address, a.maxAddresses)
	for i := range addressSpace {
		addr, err := client.GetNewAddress()
		if err != nil {
			log.Printf("%s: Cannot create address #%d", rpcConf.Host, i+1)
			return err
		}
		addressSpace[i] = addr
	}

	if err := a.client.WalletPassphrase(a.args.walletPassphrase, timeoutSecs); err != nil {
		log.Printf("%s: Cannot unlock wallet: %v", rpcConf.Host, err)
		return err
	}

	// Send a random address upstream that will be used by the cpu miner.
	a.upstream <- addressSpace[rand.Int()%a.maxAddresses]

	select {
	// Wait for matured coinbase
	case <-spendAfter:
		log.Println("Start spending funds")
	case <-a.quit:
		// If the simulation ends for any reason before the actor's coinbase
		// matures, we don't want it to get stuck on spendAfter.
		return nil
	}

	// Start a goroutine to send addresses upstream.
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		for {
			select {
			case a.upstream <- addressSpace[rand.Int()%a.maxAddresses]:
				// Send address to upstream to request receiving a transaction.
			case <-a.quit:
				return
			}
		}
	}()

	// utxo is used to transfer utxo results from the main actor goroutine to
	// the goroutine responsible for creating and sending raw transactions.
	utxo := make(chan btcjson.ListUnspentResult)

	// Start a goroutine to send transactions.
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()

		for {
			select {
			case tx := <-utxo:
				// Create a raw transaction
				inputs := []btcjson.TransactionInput{{tx.TxId, tx.Vout}}
				amt, _ := btcutil.NewAmount(tx.Amount)
				var addr btcutil.Address

				select {
				case addr = <-a.downstream:
				case <-a.quit:
					return
				}

				amounts := map[btcutil.Address]btcutil.Amount{addr: amt}
				msgTx, err := a.client.CreateRawTransaction(inputs, amounts)
				if err != nil {
					log.Printf("%s: Cannot create raw transaction: %v", rpcConf.Host, err)
					continue
				}
				// sign it
				msgTx, ok, err := a.client.SignRawTransaction(msgTx)
				if err != nil {
					log.Printf("%s: Cannot sign raw transaction: %v", rpcConf.Host, err)
					continue
				}
				if !ok {
					log.Printf("%s: Not all inputs have been signed", rpcConf.Host)
					continue
				}
				// and finally send it.
				if _, err := a.client.SendRawTransaction(msgTx, false); err != nil {
					log.Printf("%s: Cannot send raw transaction: %v", rpcConf.Host, err)
					continue
				}
			case <-a.quit:
				return
			}
		}
	}()

out:
	// List unspent transactions.
	for {
		unspent, err := a.client.ListUnspent()
		if err != nil {
			log.Printf("%s: Cannot list transactions: %v", rpcConf.Host, err)
			return err
		}

		// Search for eligible utxos
		for _, u := range unspent {
			if isMatureCoinbase(&u) || isNotCoinbase(&u) {
				select {
				case utxo <- u:
					// Send an eligible utxo result to the goroutine responsible
					// for sending transactions.
				case <-a.quit:
					break out
				}
			}
		}

		// Non-blocking check of quit channel
		select {
		case <-a.quit:
			break out
		default:
		}
	}

	return nil
}

// Stop closes the client and quit channel so every running goroutine
// can return or just exits if quit has already been closed.
func (a *Actor) Stop() {
	select {
	case <-a.quit:
	default:
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
		log.Printf("Actor on %s shutdown successfully", "localhost:"+a.args.port)
		if err := Exit(a.cmd); err != nil {
			log.Printf("Cannot exit actor on %s: %v", "localhost:"+a.args.port, err)
		}
		if err := a.Cleanup(); err != nil {
			log.Printf("Cannot cleanup actor directory on %s: %v", "localhost:"+a.args.port, err)
		}
		a.closed = true
	} else {
		log.Printf("Actor on %s already shutdown", "localhost:"+a.args.port)
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

// isMatureCoinbase checks if a coinbase transaction has reached maturity
func isMatureCoinbase(tx *btcjson.ListUnspentResult) bool {
	return tx.Confirmations >= 100
}

// isNotCoinbase checks if a utxo is not a coinbase
func isNotCoinbase(tx *btcjson.ListUnspentResult) bool {
	return tx.Vout > 0
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
