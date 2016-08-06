// Copyright (c) 2014-2016 The btcsuite developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package main

import (
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/davecgh/go-spew/spew"
)

// fakeReader implements the io.Reader interface and is used to force
// read errors
type fakeReader struct {
	n   int
	err error
}

// Read returns the fake reader error and the lesser of the fake reader value
// and the length of p.
func (r *fakeReader) Read(p []byte) (int, error) {
	n := r.n
	if n > len(p) {
		n = len(p)
	}
	return n, r.err
}

var fakeCSV = `
20000,40000,20000
`

var fakeInvalidCSV = `
foo,bar,spam
`

func TestReadCSV(t *testing.T) {
	txCurve, err := readCSV(strings.NewReader(fakeCSV))
	if err != nil {
		t.Errorf("readCSV error: %v", err)
	}
	expectedTxCurve := map[int32]*Row{
		20000: &Row{
			utxoCount: 40000,
			txCount:   20000,
		},
	}
	if !reflect.DeepEqual(txCurve, expectedTxCurve) {
		t.Errorf("readCSV got: %v want: %v", spew.Sdump(txCurve), spew.Sdump(expectedTxCurve))
	}
}

func TestReadCSVErrors(t *testing.T) {
	_, err := readCSV(&fakeReader{n: 0, err: io.ErrClosedPipe})
	if err == nil {
		t.Errorf("readCSV expected error, got %v", err)
	}
	_, err = readCSV(strings.NewReader(fakeInvalidCSV))
	if err == nil {
		t.Errorf("readCSV expected error, got %v", err)
	}
}
