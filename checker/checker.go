package checker

import (
	"context"
	"github.com/golang/glog"
	probing "github.com/prometheus-community/pro-bing"
	"runtime"
	"sync"
	"time"
)

type PingErrorType int

const (
	PingErrorNo     PingErrorType = 0
	PingErrorSystem PingErrorType = 1
	PingErrorServer PingErrorType = 2
)

type Checker struct {
	PingInterval time.Duration
	Privileged   bool

	resultCh chan *Result

	mutex       sync.Mutex
	pingServers []string
	badServers  map[string]time.Time
}

type Result struct {
	Time   time.Time
	Server string
	Rtt    time.Duration
}

const (
	defaultPingInterval = time.Millisecond * 500

	badServerCheckDelay = 1 * time.Minute
	badServerPingCount  = 3
)

func NewChecker(pingServers []string) *Checker {
	return &Checker{
		PingInterval: defaultPingInterval,
		pingServers:  pingServers,
		badServers:   make(map[string]time.Time),
	}
}

// Ping sends a ping to the specified server and returns the response time.
func (c *Checker) Ping(parentCtx context.Context, server string) (time.Duration, PingErrorType, error) {
	var success bool
	var recvPkt probing.Packet

	ctx, cancel := context.WithTimeout(parentCtx, time.Second)
	defer cancel()

	pinger, err := probing.NewPinger(server)
	if err != nil {
		return 0, PingErrorSystem, err
	}
	if runtime.GOOS == "windows" {
		pinger.SetPrivileged(true)
	} else {
		pinger.SetPrivileged(c.Privileged)
	}

	pinger.Count = 1

	pinger.OnRecv = func(pkt *probing.Packet) {
		success = true
		recvPkt = *pkt
	}

	pinger.OnFinish = func(stats *probing.Statistics) {
		cancel()
	}

	err = pinger.RunWithContext(ctx)
	if err != nil {
		return 0, PingErrorSystem, err
	}

	_, _ = <-ctx.Done()

	if success {
		return recvPkt.Rtt, PingErrorNo, nil
	}
	return 0, PingErrorServer, nil
}

// monitorServers sends pings to the servers sequentially, skipping badServers.
func (c *Checker) monitorServers(ctx context.Context, resultCh chan *Result) {
	failCount := make(map[string]int)
	serverIndex := 0

	for ctx.Err() == nil {
		c.mutex.Lock()
		server := c.pingServers[serverIndex]

		// Skip bad servers
		if _, isBad := c.badServers[server]; isBad {
			serverIndex = (serverIndex + 1) % len(c.pingServers)
			c.mutex.Unlock()
			continue
		}
		c.mutex.Unlock()

		now := time.Now()
		rtt, errType, err := c.Ping(ctx, server)
		// glog.Infof("Ping to %s : result=%v, rtt=%v\n", server, errType, rtt)
		if errType == PingErrorSystem {
			glog.Warningf("ping operation failed: %v", err)
		} else if errType != PingErrorNo {
			resultCh <- &Result{
				Time:   now,
				Server: server,
				Rtt:    -100 * time.Millisecond,
			}

			failCount[server]++

			// Ping the server and measure response time
			if failCount[server] >= badServerPingCount {
				go c.addBadServer(server)
				failCount[server] = 0
			}
		} else {
			resultCh <- &Result{
				Time:   now,
				Server: server,
				Rtt:    rtt,
			}
			failCount[server] = 0
		}

		serverIndex = (serverIndex + 1) % len(c.pingServers)

		time.Sleep(c.PingInterval)
	}
}

// checkBadServers checks the badServers after a delay to see if they have recovered.
func (c *Checker) checkBadServers(ctx context.Context) {
	doneCh := ctx.Done()

	for ctx.Err() == nil {
		var serversToCheck []string

		// Copy badServers to a local list to minimize lock time
		c.mutex.Lock()
		for server, lastFailure := range c.badServers {
			if time.Since(lastFailure) >= badServerCheckDelay {
				serversToCheck = append(serversToCheck, server)
			}
		}
		c.mutex.Unlock()

		// Check the servers that were marked as bad
		for _, server := range serversToCheck {
			_, errType, err := c.Ping(ctx, server)
			if errType == PingErrorSystem {
				glog.Warningf("ping operation failed: %v", err)
			} else if errType != PingErrorNo {
				glog.Infof("Ping to %s still failed: %v\n", server, errType)
			} else {
				glog.Infof("Ping to %s recovered\n", server)

				// Remove the server from badServers if recovered
				c.mutex.Lock()
				delete(c.badServers, server)
				c.mutex.Unlock()
			}
		}

		select {
		case <-doneCh:
		case <-time.After(badServerCheckDelay / 2):
		}
	}
}

func (c *Checker) addBadServer(server string) {
	c.mutex.Lock()
	c.badServers[server] = time.Now()
	c.mutex.Unlock()
}

func (c *Checker) Run(ctx context.Context, resultCh chan *Result) {
	var wg sync.WaitGroup

	wg.Add(2)
	go func() {
		defer wg.Done()
		c.monitorServers(ctx, resultCh)
	}()
	go func() {
		defer wg.Done()
		c.checkBadServers(ctx)
	}()

	wg.Wait()
}
