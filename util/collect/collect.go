/*
 * Copyright (c) 2013, 2014 Conformal Systems LLC <info@conformal.com>
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

package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"
)

func main() {
	stats := flag.String("stats", "../../btcsim-stats.txt", "file to parse and extract simulation data")
	verbose := flag.Bool("verbose", false, "determine verboseness of output")
	flag.Parse()

	file, err := os.Open(*(stats))
	if err != nil {
		log.Fatalf("Failed to open file: %v", err)
	}
	defer file.Close()

	parseFile(file, verbose)
}

func parseFile(file io.Reader, verbose *bool) {
	reader := bufio.NewReader(file)
	data := make([]float64, 10)
	actors := make([]int, 10)

	a, _ := regexp.Compile("actors: [0-9]+")
	t, _ := regexp.Compile("tps: [0-9]+.[0-9]+")

	for {
		line, readErr := reader.ReadString('\n')
		if readErr != nil { // including EOF
			break
		}
		line = strings.TrimSpace(line)

		if len(line) == 0 {
			continue
		}

		actorString := strings.Split(a.FindString(line), " ")
		num, err := strconv.Atoi(actorString[len(actorString)-1])
		if err != nil {
			log.Printf("Cannot convert %s to integer: %v", len(actorString)-1, err)
			continue
		}

		tpsString := strings.Split(t.FindString(line), " ")
		tps, err := strconv.ParseFloat(tpsString[len(tpsString)-1], 64)
		if err != nil {
			log.Printf("Cannot convert %s to float: %v", len(tpsString)-1, err)
			continue
		}

		// actors must grow
		if num >= len(actors) {
			newSlice := make([]int, num+1)
			copy(newSlice, actors)
			actors = newSlice
		}
		actors[num]++

		// data must grow
		if num >= len(data) {
			newSlice := make([]float64, num+1)
			copy(newSlice, data)
			data = newSlice
		}
		data[num] += tps
	}

	for num, simulations := range actors {
		if simulations == 0 {
			continue
		}

		data[num] /= float64(simulations)
		if *verbose {
			fmt.Printf("actors: %d, tps: %.2f, sims: %d\n", num, data[num], simulations)
		} else {
			fmt.Printf("actors: %d, tps: %.2f\n", num, data[num])
		}
	}

	return
}
