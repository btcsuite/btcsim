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
	"os"
	"testing"
)

func TestnewBtcwalletArgs(t *testing.T) {
	certFile := "rpc.cert"
	keyFile := "rpc.key"

	btcdArgs, err := NewBtcdArgs("node", certFile, keyFile)
	args, err := NewBtcwalletArgs(18554, btcdArgs)
	defer btcdArgs.Cleanup()
	defer args.Cleanup()
	if err != nil {
		t.Errorf("newBtcwalletArgs error: %v", err)
	}
	defer os.Remove(certFile)
	defer os.Remove(keyFile)
	expectedArgs := &BtcwalletArgs{
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
