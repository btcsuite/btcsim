// Copyright (c) 2014 Conformal Systems LLC.
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package main

import (
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"time"

	rpc "github.com/conformal/btcrpcclient"
	"github.com/conformal/btcutil"
	"github.com/conformal/btcwire"
)

// Miner holds all the core features required to register, run, control,
// and kill a cpu-mining btcd instance.
type Miner struct {
	cmd     *exec.Cmd
	client  *rpc.Client
	datadir string
	logdir  string
	closed  bool
}

// NewMiner starts a cpu-mining enabled btcd instane and returns an rpc client
// to control it.
func NewMiner(addressTable []btcutil.Address, stop chan struct{}, currentBlock int32) (*Miner, error) {

	datadir, err := ioutil.TempDir("", "minerData")
	if err != nil {
		return nil, err
	}
	logdir, err := ioutil.TempDir("", "minerLogs")
	if err != nil {
		return nil, err
	}

	miner := &Miner{
		datadir: datadir,
		logdir:  logdir,
	}

	minerArgs := []string{
		"--simnet",
		"-u" + defaultChainServer.user,
		"-P" + defaultChainServer.pass,
		"--datadir=" + miner.datadir,
		"--logdir=" + miner.logdir,
		"--rpccert=" + defaultChainServer.certPath,
		"--rpckey=" + defaultChainServer.keyPath,
		"--listen=:18550",
		"--rpclisten=:18551",
		"--generate",
	}

	for _, addr := range addressTable {
		minerArgs = append(minerArgs, "--miningaddr="+addr.EncodeAddress())
	}

	miner.cmd = exec.Command("btcd", minerArgs...)
	if err := miner.cmd.Start(); err != nil {
		log.Printf("%s: Cannot start mining: %v", defaultChainServer.connect, err)
		return nil, err
	}

	// RPC mining client initialization.
	rpcConf := rpc.ConnConfig{
		Host:         "localhost:18551",
		Endpoint:     "ws",
		User:         defaultChainServer.user,
		Pass:         defaultChainServer.pass,
		Certificates: defaultChainServer.cert,
	}

	ntfnHandlers := rpc.NotificationHandlers{
		// When a block higher than maxBlocks connects to the chain,
		// send a signal to stop actors. This is used so main can break from
		// select and call actor.Stop to stop actors.
		OnBlockConnected: func(hash *btcwire.ShaHash, height int32) {
			if height == int32(*maxBlocks) {
				close(stop)
			}
		},
	}

	var client *rpc.Client
	for i := 0; i < *maxConnRetries; i++ {
		if client, err = rpc.New(&rpcConf, &ntfnHandlers); err != nil {
			time.Sleep(time.Duration(i) * 50 * time.Millisecond)
			continue
		}
		miner.client = client
		break
	}
	if miner.client == nil {
		log.Printf("Cannot start mining rpc client: %v", err)
		return miner, err
	}

	// Use just one core for mining.
	miner.client.SetGenerate(true, 1)

	// Register for block notifications.
	if err := miner.client.NotifyBlocks(); err != nil {
		log.Printf("Cannot register for block notifications: %v", err)
		return miner, err
	}

	log.Println("CPU mining started")
	return miner, nil
}

// Shutdown kills the mining btcd process and removes its data and
// log directories.
func (m *Miner) Shutdown() {
	if !m.closed {
		if err := Exit(m.cmd); err != nil {
			log.Printf("Cannot kill mining btcd process: %v", err)
			return
		}

		if err := os.RemoveAll(m.datadir); err != nil {
			log.Printf("Cannot remove mining btcd datadir: %v", err)
			return
		}
		if err := os.RemoveAll(m.logdir); err != nil {
			log.Printf("Cannot remove mining btcd logdir: %v", err)
			return
		}

		m.closed = true
		log.Println("Miner shutdown successfully")
	} else {
		log.Println("Miner already shutdown")
	}
}
