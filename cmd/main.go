package main

import (
	"context"
	"flag"
	influxdb2 "github.com/influxdata/influxdb-client-go/v2"
	"github.com/influxdata/influxdb-client-go/v2/api"
	"github.com/jc-lab/ping-watcher/checker"
	"log"
	"os"
	"strings"
	"syscall"
	"time"
)

func init() {
	flag.Set("logtostderr", "true")
}

func main() {
	var device string
	var servers string
	var influxdbServer string
	var influxdbToken string
	var influxdbOrg string
	var influxdbBucket string
	var privileged bool
	var interval float64
	flag.StringVar(&device, "device", "", "device name")
	flag.StringVar(&servers, "servers", "8.8.8.8,8.8.4.4,1.1.1.1,168.126.63.1,168.126.63.2", "PING_SERVERS")
	flag.StringVar(&influxdbServer, "influxdb-server", "", "INFLUXDB_SERVER")
	flag.StringVar(&influxdbToken, "influxdb-token", "", "INFLUXDB_TOKEN")
	flag.StringVar(&influxdbOrg, "influxdb-org", "", "INFLUXDB_ORG")
	flag.StringVar(&influxdbBucket, "influxdb-bucket", "", "INFLUXDB_BUCKET")
	flag.BoolVar(&privileged, "privileged", false, "use privileged mode")
	flag.Float64Var(&interval, "interval", 0.5, "interval (seconds)")
	flag.Parse()

	if device == "" {
		device, _ = os.Hostname()
	}
	useEnv(&servers, "PING_SERVERS")
	useEnv(&influxdbServer, "INFLUXDB_SERVER")
	useEnv(&influxdbToken, "INFLUXDB_TOKEN")
	useEnv(&influxdbOrg, "INFLUXDB_ORG")
	useEnv(&influxdbBucket, "INFLUXDB_BUCKET")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigs
		log.Printf("catch signal: %+v", sig)
		cancel()
	}()

	client := influxdb2.NewClient(influxdbServer, influxdbToken)
	defer client.Close()

	writeAPI := client.WriteAPIBlocking(influxdbOrg, influxdbBucket)

	c := checker.NewChecker(strings.Split(servers, ","))
	c.Privileged = privileged
	c.PingInterval = time.Duration(interval*1000) * time.Millisecond

	resultCh := make(chan *checker.Result, 10)
	go func() {
		doneCh := ctx.Done()

		for ctx.Err() == nil {
			select {
			case result := <-resultCh:
				writeToInfluxDB(writeAPI, device, result)
			case _, _ = <-doneCh:
			}
		}
	}()

	log.Println("START")
	c.Run(ctx, resultCh)
}

func useEnv(p *string, name string) {
	if *p == "" {
		*p = os.Getenv(name)
	}
}

func writeToInfluxDB(writeAPI api.WriteAPIBlocking, device string, result *checker.Result) {
	p := influxdb2.NewPointWithMeasurement("ping").
		AddTag("device", device).
		AddTag("server", result.Server).
		AddField("duration", result.Rtt.Seconds()).
		SetTime(result.Time)
	if err := writeAPI.WritePoint(context.Background(), p); err != nil {
		log.Println("Error writing to InfluxDB:", err)
	}
}
