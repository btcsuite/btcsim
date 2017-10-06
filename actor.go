// Copyright (c) 2014-2016 The btcsuite developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcjson"
	rpc "github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
)

// minFee is the minimum tx fee that can be paid
const minFee btcutil.Amount = 1e4 // 0.0001 BTC

// utxoQueue is the queue of utxos belonging to a actor
// utxos are queued after a block is received and are dispatched
// to their respective owner from com.poolUtxos
// they are dequeued from simulateTx and splitUtxos
type utxoQueue struct {
	utxos   []*TxOut
	enqueue chan *TxOut
	dequeue chan *TxOut
}

// Actor describes an actor on the simulation network.  Each actor runs
// independantly without external input to decide it's behavior.
type Actor struct {
	*Node
	quit             chan struct{}
	wg               sync.WaitGroup
	ownedAddresses   []btcutil.Address
	utxoQueue        *utxoQueue
	miningAddr       chan btcutil.Address
	walletPassphrase string
}

// TxOut is a valid tx output that can be used to generate transactions
type TxOut struct {
	OutPoint *wire.OutPoint
	Amount   btcutil.Amount
}

// NewActor creates a new actor which runs its own wallet process connecting
// to the btcd node server specified by node, and listening for simulator
// websocket connections on the specified port.
func NewActor(node *Node, port uint16) (*Actor, error) {
	// Please don't run this as root.
	if port < 1024 {
		return nil, errors.New("invalid actor port")
	}

	// Set btcwallet node args
	args, err := newBtcwalletArgs(port, node.Args.(*btcdArgs))
	if err != nil {
		return nil, err
	}

	logFile, err := getLogFile(args.prefix)
	if err != nil {
		log.Printf("Cannot get log file, logging disabled: %v", err)
	}
	btcwallet, err := NewNodeFromArgs(args, nil, logFile)
	if err != nil {
		return nil, err
	}

	a := Actor{
		Node:             btcwallet,
		quit:             make(chan struct{}),
		ownedAddresses:   make([]btcutil.Address, *maxAddresses),
		miningAddr:       make(chan btcutil.Address),
		walletPassphrase: "password",
		utxoQueue: &utxoQueue{
			enqueue: make(chan *TxOut),
			dequeue: make(chan *TxOut),
		},
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
	connected := make(chan struct{})
	const timeoutSecs int64 = 3600 * 24

	if err := a.Node.Start(); err != nil {
		a.Shutdown()
		com.errChan <- struct{}{}
		return err
	}

	ntfnHandlers := &rpc.NotificationHandlers{
		OnClientConnected: func() {
			connected <- struct{}{}
		},
	}
	a.handlers = ntfnHandlers

	if err := a.Connect(); err != nil {
		a.Shutdown()
		com.errChan <- struct{}{}
		return err
	}

	// Wait for btcd to connect
	<-connected

	// Wait for wallet sync
	for i := 0; i < *maxConnRetries; i++ {
		if _, err := a.client.GetBalance(""); err != nil {
			time.Sleep(time.Duration(i) * 50 * time.Millisecond)
			continue
		}
		break
	}

	// Create wallet addresses and unlock wallet.
	log.Printf("%s: Creating wallet addresses...", a)
	for i := range a.ownedAddresses {
		fmt.Printf("\r%d/%d", i+1, len(a.ownedAddresses))
		addr, err := a.client.GetNewAddress("default")
		if err != nil {
			log.Printf("%s: Cannot create address #%d", a, i+1)
			com.errChan <- struct{}{}
			return err
		}
		a.ownedAddresses[i] = addr
	}
	fmt.Printf("\n")

	if err := a.client.WalletPassphrase(a.walletPassphrase, timeoutSecs); err != nil {
		log.Printf("%s: Cannot unlock wallet: %v", a, err)
		com.errChan <- struct{}{}
		return err
	}

	// Send a random address that will be used by the cpu miner.
	a.miningAddr <- a.ownedAddresses[rand.Int()%len(a.ownedAddresses)]

	// Start a goroutine that queues up a set of utxos belonging to this
	// actor. The utxos are sent from com.poolUtxos which in turn receives
	// block notifications from sim.go
	a.wg.Add(1)
	go a.queueUtxos()

	// Start a goroutine to simulate transactions.
	a.wg.Add(1)
	go a.simulateTx(com.downstream, com.txpool)

	// Start a goroutine to split utxos
	a.wg.Add(1)
	go a.splitUtxos(com.split, com.txpool)

	return nil
}

// simulateTx runs as a goroutine and simulates transactions between actors
//
// It receives a random address downstream, dequeues a utxo, sends a raw
// transaction to the address using the utxo as input
func (a *Actor) simulateTx(downstream <-chan btcutil.Address, txpool chan<- struct{}) {
	defer a.wg.Done()

	for {
		select {
		case utxo := <-a.utxoQueue.dequeue:
			select {
			case addr := <-downstream:
				// Create a raw transaction
				inputs := []btcjson.TransactionInput{{
					Txid: utxo.OutPoint.Hash.String(),
					Vout: utxo.OutPoint.Index,
				}}

				// Provide a fees of minFee to ensure the tx gets mined
				// the utxo amount is guaranteed to be > maxSplit*minFee
				amt := utxo.Amount - minFee
				amounts := map[btcutil.Address]btcutil.Amount{
					addr: amt,
				}

				err := a.sendRawTransaction(inputs, amounts)
				if err != nil {
					log.Printf("%s: Error sending raw transaction: %v", a, err)
					select {
					case txpool <- struct{}{}:
					case <-a.quit:
						return
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

// splitUtxos runs as a goroutine and builds up a large set of utxos that
// can be used to simulate large tx/block ratios
//
// It receives a 'split' which is int that indicates the number of resultant utxos
// the tx is sent to addresses from the same actor since we're only interested in
// building up the utxo set
func (a *Actor) splitUtxos(split <-chan int, txpool chan<- struct{}) {
	defer a.wg.Done()

	for {
		select {
		case utxo := <-a.utxoQueue.dequeue:
			select {
			case split := <-split:
				// Create a raw transaction
				inputs := []btcjson.TransactionInput{{
					Txid: utxo.OutPoint.Hash.String(),
					Vout: utxo.OutPoint.Index,
				}}

				// Provide a fees of minFee to ensure the tx gets mined
				// the utxo amount is guaranteed to be > maxSplit*minFee
				amt := utxo.Amount - minFee
				amounts := map[btcutil.Address]btcutil.Amount{}

				// Create a output of random amount and sent it
				// to a random address from the address space
				// total number of outputs is split+1, taking into
				// account this utxo which is consumed in the process

				// set a rand start index for getting different random addrs
				randomIndex := rand.Int() % len(a.ownedAddresses)
				for i := 0; i <= split; i++ {
					var to btcutil.Address
					var change btcutil.Amount
					// pick a random address
					if randomIndex+i < len(a.ownedAddresses) {
						to = a.ownedAddresses[randomIndex+i]
					} else {
						// wrap around incase of an overflow
						to = a.ownedAddresses[randomIndex+i-len(a.ownedAddresses)]
					}
					if i == split {
						// last split, so just set the change
						change = amt
					} else {
						// pick a random change amount which is less than amt
						// but have a lower bound at minFee
						change = btcutil.Amount(rand.Int63n(int64(amt) / 2))
						if change < minFee {
							change = minFee
						}
						amt -= change
					}
					amounts[to] = change
				}

				err := a.sendRawTransaction(inputs, amounts)
				if err != nil {
					log.Printf("%s: Error sending raw transaction: %v", a, err)
					select {
					case txpool <- struct{}{}:
					case <-a.quit:
						return
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

// sendRawTransaction creates a raw transaction, signs it and sends it
func (a *Actor) sendRawTransaction(inputs []btcjson.TransactionInput, amounts map[btcutil.Address]btcutil.Amount) error {
	msgTx, err := a.client.CreateRawTransaction(inputs, amounts, nil)
	if err != nil {
		return err
	}
	// sign it
	msgTx, ok, err := a.client.SignRawTransaction(msgTx)
	if err != nil {
		return err
	}
	if !ok {
		return err
	}
	// and finally send it.
	if _, err := a.client.SendRawTransaction(msgTx, false); err != nil {
		return err
	}
	return nil
}

// queueUtxos receives utxos belonging to this actor and queues them up
func (a *Actor) queueUtxos() {
	defer a.wg.Done()

	enqueue := a.utxoQueue.enqueue
	var dequeue chan *TxOut
	var next *TxOut
out:
	for {
		select {
		case n, ok := <-enqueue:
			if !ok {
				// If no utxos are queued for handling,
				// the queue is finished.
				if len(a.utxoQueue.utxos) == 0 {
					break out
				}
				// nil channel so no more reads can occur.
				enqueue = nil
				continue
			}
			if len(a.utxoQueue.utxos) == 0 {
				next = n
				dequeue = a.utxoQueue.dequeue
			}
			a.utxoQueue.utxos = append(a.utxoQueue.utxos, n)
		case dequeue <- next:
			a.utxoQueue.utxos[0] = nil
			a.utxoQueue.utxos = a.utxoQueue.utxos[1:]
			if len(a.utxoQueue.utxos) != 0 {
				next = a.utxoQueue.utxos[0]
			} else {
				// If no more utxos can be enqueued, the
				// queue is finished.
				if enqueue == nil {
					break out
				}
				dequeue = nil
			}
		case <-a.quit:
			break out
		}
	}
	close(a.utxoQueue.dequeue)
}

// Shutdown performs a shutdown down the actor by first signalling
// all goroutines to stop, waiting for them to stop and them cleaning up
func (a *Actor) Shutdown() {
	select {
	case <-a.quit:
	default:
		close(a.quit)
		a.WaitForShutdown()
		a.Node.Shutdown()
	}
}

// WaitForShutdown waits until every actor goroutine has returned
func (a *Actor) WaitForShutdown() {
	a.wg.Wait()
}
