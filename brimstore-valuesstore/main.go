package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gholt/brimstore"
	"github.com/gholt/brimutil"
	"github.com/jessevdk/go-flags"
)

type optsStruct struct {
	Clients       int    `long:"clients" description:"The number of clients. Default: cores*cores"`
	Cores         int    `long:"cores" description:"The number of cores. Default: CPU core count"`
	ExtendedStats bool   `long:"extended-stats" description:"Extended statistics at exit."`
	Length        int    `short:"l" long:"length" description:"Length of values. Default: 0"`
	Number        int    `short:"n" long:"number" description:"Number of keys. Default: 0"`
	Random        int    `long:"random" description:"Random number seed. Default: 0"`
	Sequence      uint64 `long:"sequence" description:"Sequence number. Default: 2 for write, 3 for delete"`
	Positional    struct {
		Tests []string `name:"tests" description:"delete lookup read write"`
	} `positional-args:"yes"`
	keyspace []byte
	buffers  [][]byte
	value    []byte
	st       runtime.MemStats
	vs       *brimstore.ValuesStore
}

var opts optsStruct
var parser = flags.NewParser(&opts, flags.Default)

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		args = append(args, "-h")
	}
	if _, err := parser.ParseArgs(args); err != nil {
		os.Exit(1)
	}
	for _, arg := range opts.Positional.Tests {
		switch arg {
		case "delete":
		case "lookup":
		case "read":
		case "write":
		default:
			fmt.Fprintf(os.Stderr, "Unknown test named %#v.\n", arg)
			os.Exit(1)
		}
	}
	if opts.Cores > 0 {
		runtime.GOMAXPROCS(opts.Cores)
	} else if os.Getenv("GOMAXPROCS") == "" {
		runtime.GOMAXPROCS(runtime.NumCPU())
	}
	opts.Cores = runtime.GOMAXPROCS(0)
	if opts.Clients == 0 {
		opts.Clients = opts.Cores * opts.Cores
	}
	opts.keyspace = make([]byte, opts.Number*16)
	brimutil.NewSeededScrambled(int64(opts.Random)).Read(opts.keyspace)
	opts.buffers = make([][]byte, opts.Clients)
	for i := 0; i < opts.Clients; i++ {
		opts.buffers[i] = make([]byte, 4*1024*1024)
	}
	opts.value = make([]byte, opts.Length)
	brimutil.NewSeededScrambled(int64(opts.Random)).Read(opts.value)
	if len(opts.value) > 10 {
		copy(opts.value, []byte("START67890"))
	}
	if len(opts.value) > 20 {
		copy(opts.value[len(opts.value)-10:], []byte("123456STOP"))
	}
	fmt.Println(opts.Cores, "cores")
	fmt.Println(opts.Clients, "clients")
	fmt.Println(opts.Number, "values")
	fmt.Println(opts.Length, "value length")
	memstat()
	begin := time.Now()
	opts.vs = brimstore.NewValuesStore(nil)
	dur := time.Now().Sub(begin)
	fmt.Println(dur, "to start ValuesStore")
	memstat()
	for _, arg := range opts.Positional.Tests {
		switch arg {
		case "delete":
			delete()
		case "lookup":
			lookup()
		case "read":
			read()
		case "write":
			write()
		}
		memstat()
	}
	begin = time.Now()
	opts.vs.Close()
	dur = time.Now().Sub(begin)
	fmt.Println(dur, "to close ValuesStore")
	memstat()
	begin = time.Now()
	stats := opts.vs.GatherStats(opts.ExtendedStats)
	dur = time.Now().Sub(begin)
	fmt.Println(dur, "to gather stats")
	if opts.ExtendedStats {
		fmt.Println(stats.String())
	} else {
		fmt.Println(stats.ValueCount(), "ValueCount")
		fmt.Println(stats.ValuesLength(), "ValuesLength")
	}
	memstat()
}

func memstat() {
	lastAlloc := opts.st.TotalAlloc
	runtime.ReadMemStats(&opts.st)
	deltaAlloc := opts.st.TotalAlloc - lastAlloc
	lastAlloc = opts.st.TotalAlloc
	fmt.Printf("%0.2fG total alloc, %0.2fG delta\n\n", float64(opts.st.TotalAlloc)/1024/1024/1024, float64(deltaAlloc)/1024/1024/1024)
}

func delete() {
	var superseded uint64
	seq := opts.Sequence | 1
	begin := time.Now()
	wg := &sync.WaitGroup{}
	wg.Add(opts.Clients)
	for i := 0; i < opts.Clients; i++ {
		go func(client int) {
			var s uint64
			number := len(opts.keyspace) / 16
			numberPer := number / opts.Clients
			var keys []byte
			if client == opts.Clients-1 {
				keys = opts.keyspace[numberPer*client*16:]
			} else {
				keys = opts.keyspace[numberPer*client*16 : numberPer*(client+1)*16]
			}
			for o := 0; o < len(keys); o += 16 {
				if oldSeq, err := opts.vs.Delete(binary.BigEndian.Uint64(keys[o:]), binary.BigEndian.Uint64(keys[o+8:]), seq); err != nil {
					panic(err)
				} else if oldSeq > seq {
					s++
				}
			}
			if s > 0 {
				atomic.AddUint64(&superseded, s)
			}
			wg.Done()
		}(i)
	}
	wg.Wait()
	dur := time.Now().Sub(begin)
	fmt.Printf("%s %.0f/s to delete %d values (seq %d)\n", dur, float64(opts.Number)/(float64(dur)/float64(time.Second)), opts.Number, seq)
	if superseded > 0 {
		fmt.Println(superseded, "SUPERCEDED!")
	}
}

func lookup() {
	var missing uint64
	var deleted uint64
	begin := time.Now()
	wg := &sync.WaitGroup{}
	wg.Add(opts.Clients)
	for i := 0; i < opts.Clients; i++ {
		go func(client int) {
			number := len(opts.keyspace) / 16
			numberPer := number / opts.Clients
			var keys []byte
			if client == opts.Clients-1 {
				keys = opts.keyspace[numberPer*client*16:]
			} else {
				keys = opts.keyspace[numberPer*client*16 : numberPer*(client+1)*16]
			}
			var m uint64
			var d uint64
			for o := 0; o < len(keys); o += 16 {
				q, _, err := opts.vs.Lookup(binary.BigEndian.Uint64(keys[o:]), binary.BigEndian.Uint64(keys[o+8:]))
				if err == brimstore.ErrValueNotFound {
					if q == 0 {
						m++
					} else {
						d++
					}
				} else if err != nil {
					panic(err)
				}
			}
			if m > 0 {
				atomic.AddUint64(&missing, m)
			}
			if d > 0 {
				atomic.AddUint64(&deleted, d)
			}
			wg.Done()
		}(i)
	}
	wg.Wait()
	dur := time.Now().Sub(begin)
	fmt.Printf("%s %.0f/s to lookup %d values\n", dur, float64(opts.Number)/(float64(dur)/float64(time.Second)), opts.Number)
	if missing > 0 {
		fmt.Println(missing, "MISSING!")
	}
	if deleted > 0 {
		fmt.Println(deleted, "DELETED!")
	}
}

func read() {
	var valuesLength uint64
	var missing uint64
	var deleted uint64
	start := []byte("START67890")
	stop := []byte("123456STOP")
	wg := &sync.WaitGroup{}
	wg.Add(opts.Clients)
	begin := time.Now()
	for i := 0; i < opts.Clients; i++ {
		go func(client int) {
			f := func(keys []byte) {
				var vl uint64
				var m uint64
				var d uint64
				for o := 0; o < len(keys); o += 16 {
					q, v, err := opts.vs.Read(binary.BigEndian.Uint64(keys[o:]), binary.BigEndian.Uint64(keys[o+8:]), opts.buffers[client][:0])
					if err == brimstore.ErrValueNotFound {
						if q == 0 {
							m++
						} else {
							d++
						}
					} else if err != nil {
						panic(err)
					} else if len(v) > 10 && !bytes.Equal(v[:10], start) {
						panic("bad start to value")
					} else if len(v) > 20 && !bytes.Equal(v[len(v)-10:], stop) {
						panic("bad stop to value")
					} else {
						vl += uint64(len(v))
					}
				}
				if vl > 0 {
					atomic.AddUint64(&valuesLength, vl)
				}
				if m > 0 {
					atomic.AddUint64(&missing, m)
				}
				if d > 0 {
					atomic.AddUint64(&deleted, d)
				}
			}
			number := len(opts.keyspace) / 16
			numberPer := number / opts.Clients
			var keys []byte
			if client == opts.Clients-1 {
				keys = opts.keyspace[numberPer*client*16:]
			} else {
				keys = opts.keyspace[numberPer*client*16 : numberPer*(client+1)*16]
			}
			keysplit := len(keys) / 16 / opts.Clients * client * 16
			f(keys[:keysplit])
			f(keys[keysplit:])
			wg.Done()
		}(i)
	}
	wg.Wait()
	dur := time.Now().Sub(begin)
	fmt.Printf("%s %.0f/s %0.2fG/s to read %d values\n", dur, float64(opts.Number)/(float64(dur)/float64(time.Second)), float64(valuesLength)/(float64(dur)/float64(time.Second))/1024/1024/1024, opts.Number)
	if missing > 0 {
		fmt.Println(missing, "MISSING!")
	}
	if deleted > 0 {
		fmt.Println(deleted, "DELETED!")
	}
}

func write() {
	var superseded uint64
	seq := opts.Sequence & 0xfffffffffffffffe
	if seq == 0 {
		seq = 2
	}
	begin := time.Now()
	wg := &sync.WaitGroup{}
	wg.Add(opts.Clients)
	for i := 0; i < opts.Clients; i++ {
		go func(client int) {
			var s uint64
			number := len(opts.keyspace) / 16
			numberPer := number / opts.Clients
			var keys []byte
			if client == opts.Clients-1 {
				keys = opts.keyspace[numberPer*client*16:]
			} else {
				keys = opts.keyspace[numberPer*client*16 : numberPer*(client+1)*16]
			}
			for o := 0; o < len(keys); o += 16 {
				if oldSeq, err := opts.vs.Write(binary.BigEndian.Uint64(keys[o:]), binary.BigEndian.Uint64(keys[o+8:]), seq, opts.value); err != nil {
					panic(err)
				} else if oldSeq > seq {
					s++
				}
			}
			if s > 0 {
				atomic.AddUint64(&superseded, s)
			}
			wg.Done()
		}(i)
	}
	wg.Wait()
	dur := time.Now().Sub(begin)
	fmt.Printf("%s %.0f/s %0.2fG/s to write %d values (seq %d)\n", dur, float64(opts.Number)/(float64(dur)/float64(time.Second)), float64(opts.Number*opts.Length)/(float64(dur)/float64(time.Second))/1024/1024/1024, opts.Number, seq)
	if superseded > 0 {
		fmt.Println(superseded, "SUPERCEDED!")
	}
}