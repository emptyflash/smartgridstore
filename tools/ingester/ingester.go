package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ceph/go-ceph/rados"
	etcd "github.com/coreos/etcd/clientv3"
	"github.com/immesys/smartgridstore/tools/ingester/upmuparser"
	"gopkg.in/btrdb.v4"

	uuid "github.com/pborman/uuid"
)

const VersionMajor = 4
const VersionMinor = 1
const VersionPatch = 1

var btrdbconn *btrdb.BTrDB
var ytagbase int = 0
var configfile []byte = nil

const NUM_RHANDLES = 16

const MANIFEST_PREFIX = "manifest/"

var INGESTER_SPACE uuid.UUID = uuid.Parse("c9bbebff-ff40-4dbe-987e-f9e96afb7a57")

var rhPool chan *rados.IOContext

func getEtcdKeySafe(ctx context.Context, etcdConn *etcd.Client, key string) []byte {
	resp, err := etcdConn.Get(context.Background(), key)
	if err != nil {
		log.Fatalf("Could not check for keys in etcd: %v", err)
	}
	if len(resp.Kvs) == 0 {
		return nil
	}
	return resp.Kvs[0].Value
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "-version" {
		fmt.Printf("%d.%d.%d\n", VersionMajor, VersionMinor, VersionPatch)
		os.Exit(0)
	}
	fmt.Printf("Booting ingester version %d.%d.%d\n", VersionMajor, VersionMinor, VersionPatch)

	var etcdPrefix string = os.Getenv("INGESTER_ETCD_CONFIG")

	var etcdEndpoint string = os.Getenv("ETCD_ENDPOINT")
	if len(etcdEndpoint) == 0 {
		etcdEndpoint = "localhost:2379"
		log.Printf("ETCD_ENDPOINT is not set; using %s", etcdEndpoint)
	}

	var etcdConfig etcd.Config = etcd.Config{Endpoints: []string{etcdEndpoint}}

	log.Println("Connecting to etcd...")
	etcdConn, err := etcd.New(etcdConfig)
	if err != nil {
		log.Fatalf("Error: %v", err)
	}
	defer etcdConn.Close()

	conn, err := rados.NewConn()
	if err != nil {
		fmt.Printf("Could not initialize ceph storage: %v\n", err)
		return
	}
	err = conn.ReadDefaultConfigFile()
	if err != nil {
		fmt.Printf("Could not read ceph config: %v\n", err)
		return
	}
	err = conn.Connect()
	if err != nil {
		fmt.Printf("Could not initialize ceph storage: %v\n", err)
		return
	}

	pool := os.Getenv("RECEIVER_POOL")
	if pool == "" {
		pool = "receiver"
	}

	rhPool = make(chan *rados.IOContext, NUM_RHANDLES+1)

	for j := 0; j != NUM_RHANDLES; j++ {
		h, err := conn.OpenIOContext(pool)
		if err != nil {
			fmt.Printf("Could not open ceph handle: %v", err)
			return
		}
		rhPool <- h
	}

	ctx := context.Background()

	btrdbconn, err = btrdb.Connect(ctx, btrdb.EndpointsFromEnv()...)
	if err != nil {
		fmt.Printf("Error connecting to the BTrDB: %v\n", err)
		return
	}

	defer func() {
		err := btrdbconn.Disconnect()
		if err == nil {
			fmt.Println("Finished closing connection")
		} else {
			fmt.Printf("Could not close connection: %v\n", err)
		}
	}()

	var terminate bool = false

	var alive bool = true // if this were C I'd have to malloc this
	var interrupt = make(chan os.Signal)
	signal.Notify(interrupt, os.Interrupt)
	go func() {
		for {
			<-interrupt // block until an interrupt happens
			fmt.Println("\nDetected ^C. Waiting for pending tasks to complete...")
			alive = false
		}
	}()

	wch := etcdConn.Watch(ctx, etcdPrefix+MANIFEST_PREFIX, etcd.WithPrefix())

	/* Start over if the configuration file changes */
	go func() {
		for resp := range wch {
			err := resp.Err()
			if err != nil {
				panic(err)
			}
			fmt.Println("Configuration file changed. Restarting in 15 seconds...")
			time.Sleep(15 * time.Second)
			terminate = false
			alive = false
		}
	}()

	for !terminate {
		// If we die, just terminate (unless this is set differently)
		alive = true
		terminate = true

		getEtcdKeySafe(ctx, etcdConn, etcdPrefix+"ingester/ytagbase")

		runtime.GOMAXPROCS(runtime.NumCPU())

		var num_uPMUs int = 0
		var uuids []string
		var ytagnum int64

		ytagbytes := getEtcdKeySafe(ctx, etcdConn, etcdPrefix+"ingester/generation")
		if ytagbytes != nil {
			ytagnum, err = strconv.ParseInt(string(ytagbytes), 0, 32)
			if err != nil {
				fmt.Println("generation must be an integer")
			} else {
				ytagbase = int(ytagnum)
			}
		} else {
			fmt.Println("Configuration file does not specify ytagbase. Defaulting to 10.")
			ytagbase = 10
		}

		resp, err := etcdConn.Get(ctx, etcdPrefix+MANIFEST_PREFIX+"psl.pqube3.", etcd.WithPrefix())
		if err != nil {
			panic(err)
		}
		wg := &sync.WaitGroup{}
		for _, kv := range resp.Kvs {
			key := string(kv.Key)
			identifier := key[len(etcdPrefix)+len(MANIFEST_PREFIX):]
			serial := strings.SplitN(identifier, ".", 3)[2]
			uuids = make([]string, 0, len(upmuparser.STREAMS))
			for _, canonical := range upmuparser.STREAMS {
				uuid := uuid.NewSHA1(INGESTER_SPACE, []byte(fmt.Sprintf("%s.%s", identifier, canonical)))
				uuids = append(uuids, uuid.String())
			}
			wg.Add(1)
			fmt.Printf("Starting process loop of uPMU %v\n", identifier)
			go startProcessLoop(ctx, serial, identifier, uuids, &alive, wg)
			num_uPMUs++
		}

		if num_uPMUs == 0 {
			fmt.Println("WARNING: No uPMUs found. Sleeping forever...")
			for alive {
				time.Sleep(time.Second)
			}
		} else {
			wg.Wait()
		}
	}
}

func startProcessLoop(ctx context.Context, serial_number string, alias string, uuid_strings []string, alivePtr *bool, wg *sync.WaitGroup) {
	var uuids = make([]uuid.UUID, len(uuid_strings))

	var i int

	for i = 0; i < len(uuids); i++ {
		uuids[i] = uuid.Parse(uuid_strings[i])
	}
	mgo_addr := os.Getenv("MONGO_ADDR")
	if mgo_addr == "" {
		mgo_addr = "localhost:27017"
	}

	process_loop(ctx, alivePtr, serial_number, alias, uuids, btrdbconn)

	wg.Done()
}

func insert_stream(ctx context.Context, stream *btrdb.Stream, output *upmuparser.Sync_Output, getValue upmuparser.InsertGetter, startTime int64, bc *btrdb.BTrDB, feedback chan int) {
	var sampleRate float32 = output.SampleRate()
	var numPoints int = int((1000.0 / sampleRate) + 0.5)
	var timeDelta float64 = float64(sampleRate) * 1000000 // convert to nanoseconds

	points := make([]btrdb.RawPoint, numPoints)
	for i := 0; i != len(points); i++ {
		points[i].Time = startTime + int64((float64(i)*timeDelta)+0.5)
		points[i].Value = getValue(i, output)
	}

	err := stream.Insert(ctx, points)
	if err == nil {
		feedback <- 0
	} else {
		fmt.Printf("Error inserting data: %v\n", err)
		feedback <- 1
	}
}

func process(ctx context.Context, sernum string, alias string, streams []*btrdb.Stream, bc *btrdb.BTrDB, alive *bool) bool {
	rh := <-rhPool
	defer func() { rhPool <- rh }()
	oid := fmt.Sprintf("meta.gen.%d", ytagbase)
	prefix := fmt.Sprintf("data.psl.pqube3.%s", sernum)
	todo, err := rh.GetOmapValues(oid, "", prefix, 100)
	if err != nil {
		fmt.Printf("Could not check for additional files for uPMU %v: %v\nTerminating program...\n", alias, err)
		*alive = false
		return false
	}

	documentsFound := (len(todo) != 0)

	var parsed []*upmuparser.Sync_Output
	var synco *upmuparser.Sync_Output
	var timeArr [6]int32
	var i int
	var j int
	var numsent int
	var timestamp int64
	var feedback chan int
	var success bool
	var igs []upmuparser.InsertGetter
	var ig upmuparser.InsertGetter

	for objname, _ := range todo {
		parts := strings.SplitN(objname, ".", 3)
		if len(parts) != 3 {
			fmt.Printf("Invalid object name %s\n", parts)
			continue
		}
		filename := parts[2]

		stat, err := rh.Stat(objname)
		if err != nil {
			fmt.Printf("Could not stat object: %v\n", err)
			fmt.Println("Skipping...")
			continue
		}
		rawdata := make([]byte, stat.Size, stat.Size)

		read, err := rh.Read(objname, rawdata, 0)
		if read != int(stat.Size) || err != nil {
			fmt.Printf("Could not read object: read %d out of %v bytes, err = %v\n", read, stat.Size, err)
			fmt.Println("Skipping...")
			continue
		}

		success = true
		parsed, err = upmuparser.ParseSyncOutArray(rawdata)
		feedback = make(chan int)
		numsent = 0
		for i = 0; i < len(parsed); i++ {
			synco = parsed[i]
			if synco == nil {
				var file *os.File
				fmt.Printf("Could not parse set at index %d in file %s from uPMU %s (serial=%s). Reason: %v\n", i, filename, alias, sernum, err)
				if err == io.ErrUnexpectedEOF {
					fmt.Println("Warning: skipping partially written/corrupt set...")
					continue
				} else {
					fmt.Println("Dumping bad file into error.dat...")
					file, err = os.OpenFile("error.dat", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0660)
					if err == nil {
						_, err = file.Write(rawdata)
					}
					if err == nil {
						err = file.Close()
					}
					if err == nil {
						fmt.Println("Finished writing file.")
					} else {
						fmt.Printf("Could not dump bad file: %v\n", err)
					}
					os.Exit(1)
				}
			}
			timeArr = synco.Times()
			if timeArr[0] < 2010 || timeArr[0] > 2020 {
				// if the year is outside of this range things must have gotten corrupted somehow
				fmt.Printf("Rejecting bad date record for %v: year is %v\n", alias, timeArr[0])
				continue
			}
			timestamp = time.Date(int(timeArr[0]), time.Month(timeArr[1]), int(timeArr[2]), int(timeArr[3]), int(timeArr[4]), int(timeArr[5]), 0, time.UTC).UnixNano()
			igs = synco.GetInsertGetters()
			for j, ig = range igs {
				if j >= len(streams) {
					fmt.Printf("Warning: data for a stream includes stream %s, but stream was not provided. Dropping data for that stream...\n", upmuparser.STREAMS[j])
					continue
				}
				go insert_stream(ctx, streams[j], synco, ig, timestamp, bc, feedback)
				numsent++
			}
		}
		for j = 0; j < numsent; j++ {
			if <-feedback == 1 {
				fmt.Printf("Warning: data for a stream could not be sent for uPMU %v (serial=%v)\n", alias, sernum)
				success = false
			}
		}
		fmt.Printf("Finished sending %v for uPMU %v (serial=%v)\n", filename, alias, sernum)

		if success {

			fmt.Printf("Removing %v for uPMU %v (serial=%v) from generation list\n", filename, alias, sernum)
			rh.RmOmapKeys(oid, []string{objname})

			if err == nil {
				fmt.Printf("Successfully updated ytag for %v for uPMU %v (serial=%v)\n", filename, alias, sernum)
			} else {
				fmt.Printf("Could not update ytag for a document for uPMU %v: %v\n", alias, err)
			}

		} else {
			fmt.Println("Document insert fails. Terminating program...")
			*alive = false
		}
		if !(*alive) {
			break
		}
	}

	return documentsFound
}

func process_loop(ctx context.Context, keepalive *bool, sernum string, alias string, uuids []uuid.UUID, bc *btrdb.BTrDB) {
	var i int
	var streams = []*btrdb.Stream{}
	for j, uu := range uuids {
		stream := bc.StreamFromUUID(uu)
		ex, err := stream.Exists(ctx)
		if err != nil {
			fmt.Printf("Could not check if stream exists in BTrDB: %v\n", err)
			os.Exit(1)
		}
		if !ex {
			stream, err = bc.Create(ctx, uu, fmt.Sprintf("psl.pqube3.%s", sernum), map[string]string{"name": upmuparser.STREAMS[j]}, nil)
			if err != nil {
				fmt.Printf("Could not create stream in BTrDB: %v\n", err)
				fmt.Println("This could mean that a stream exists in this collection and tags, but with a different UUID.")
				fmt.Println("I don't know how to deal with this and will now exit. Bye!")
				os.Exit(1)
			}
		}
		streams = append(streams, stream)
	}
	for *keepalive {
		fmt.Printf("looping %v\n", alias)
		if process(ctx, sernum, alias, streams, bc, keepalive) {
			fmt.Printf("sleeping %v\n", alias)
			time.Sleep(time.Second)
		} else {
			fmt.Printf("No documents found for %v. Waiting 100 seconds...\n", alias)
			for i = 0; i < 100 && *keepalive; i++ {
				time.Sleep(time.Second)
			}
		}
	}
	fmt.Printf("Terminated process loop for %v\n", alias)
}