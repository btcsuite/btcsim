// Copyright (c) 2014-2016 The btcsuite developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package main

import (
	"os"
	"testing"
)

func TestNewBtcwalletArgs(t *testing.T) {
	btcdArgs, err := newBtcdArgs("node")
	args, err := newBtcwalletArgs(18554, btcdArgs)
	defer btcdArgs.Cleanup()
	defer args.Cleanup()
	if err != nil {
		t.Errorf("newBtcwalletArgs error: %v", err)
	}
	defer os.Remove(CertFile)
	defer os.Remove(KeyFile)
	expectedArgs := &btcwalletArgs{
		// fixed
		RPCListen:  "127.0.0.1:18554",
		RPCConnect: "127.0.0.1:18556",
		Username:   "user",
		Password:   "pass",
		// the rest are env-dependent and variable
		// don't test these literally
		CAFile:  "/home/tuxcanfly/.btcsim/rpc.cert",
		DataDir: "/tmp/user/1000/actor-data948809262",
		LogDir:  "/tmp/user/1000/actor-logs649955253",
	}
	if len(expectedArgs.Arguments()) != len(args.Arguments()) {
		t.Errorf("newBtcwalletArgs wrong len expected: %v, got %v", len(expectedArgs.Arguments()), len(args.Arguments()))
	}
	expectedArguments := expectedArgs.Arguments()
	arguments := args.Arguments()
	for i := 0; i < 4; i++ {
		if expectedArguments[i] != arguments[i] {
			t.Errorf("newBtcwalletArgs expected: %v, got %v", expectedArguments[i], arguments[i])
		}
	}
	for i := 4; i < len(arguments); i++ {
		if arguments[i] == "" {
			t.Errorf("newBtcwalletArgs expected default value, got %v", arguments[i])
		}
	}
}
