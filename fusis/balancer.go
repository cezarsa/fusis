package fusis

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/luizbafilho/fusis/config"
	"github.com/luizbafilho/fusis/engine"
	fusis_net "github.com/luizbafilho/fusis/net"
	"github.com/luizbafilho/fusis/provider"

	"github.com/hashicorp/raft"
	"github.com/hashicorp/raft-boltdb"
	"github.com/hashicorp/serf/serf"
)

const (
	retainSnapshotCount   = 2
	raftTimeout           = 10 * time.Second
	raftRemoveGracePeriod = 5 * time.Second
)

// Balancer represents the Load Balancer
type Balancer struct {
	sync.Mutex
	eventCh chan serf.Event

	serf          *serf.Serf
	raft          *raft.Raft // The consensus mechanism
	raftPeers     raft.PeerStore
	raftStore     *raftboltdb.BoltStore
	raftInmem     *raft.InmemStore
	raftTransport *raft.NetworkTransport
	logger        *logrus.Logger
	config        *config.BalancerConfig

	engine     *engine.Engine
	provider   provider.Provider
	shutdownCh chan bool
}

// NewBalancer initializes a new balancer
//TODO: Graceful shutdown on initialization errors
func NewBalancer(config *config.BalancerConfig) (*Balancer, error) {
	provider, err := provider.New(config)
	if err != nil {
		return nil, err
	}

	engine, err := engine.New(config)
	if err != nil {
		return nil, err
	}

	balancer := &Balancer{
		eventCh:  make(chan serf.Event, 64),
		engine:   engine,
		provider: provider,
		logger:   logrus.New(),
		config:   config,
	}

	if err = balancer.setupRaft(); err != nil {
		return nil, fmt.Errorf("error setting up Raft: %v", err)
	}

	if err = balancer.setupSerf(); err != nil {
		return nil, fmt.Errorf("error setting up Serf: %v", err)
	}

	// Flushing all VIPs on the network interface
	if err := fusis_net.DelVips(balancer.config.Provider.Params["interface"]); err != nil {
		return nil, fmt.Errorf("error cleaning up network vips: %v", err)
	}

	go balancer.watchLeaderChanges()

	// Only collect stats if some interval is defined
	if config.Stats.Interval > 0 {
		go balancer.collectStats()
	}

	return balancer, nil
}

// Start starts the balancer
func (b *Balancer) setupSerf() error {
	conf := serf.DefaultConfig()
	conf.Init()
	conf.Tags["role"] = "balancer"
	conf.Tags["raft-port"] = strconv.Itoa(b.config.Ports["raft"])

	bindAddr, err := b.config.GetIpByInterface()
	if err != nil {
		return err
	}

	conf.MemberlistConfig.BindAddr = bindAddr
	conf.MemberlistConfig.BindPort = b.config.Ports["serf"]

	conf.NodeName = b.config.Name
	conf.EventCh = b.eventCh

	serf, err := serf.Create(conf)
	if err != nil {
		return err
	}

	b.serf = serf

	go b.handleEvents()

	return nil
}

func (b *Balancer) newStdLogger() *log.Logger {
	return log.New(b.logger.Writer(), "", 0)
}

func (b *Balancer) setupRaft() error {
	// Setup Raft configuration.
	raftConfig := raft.DefaultConfig()
	raftConfig.Logger = b.newStdLogger()

	raftConfig.ShutdownOnRemove = false
	// Check for any existing peers.
	peers, err := readPeersJSON(filepath.Join(b.config.ConfigPath, "peers.json"))
	if err != nil {
		return err
	}

	// Allow the node to entry single-mode, potentially electing itself, if
	// explicitly enabled and there is only 1 node in the cluster already.
	if b.config.Bootstrap && len(peers) <= 1 {
		b.logger.Infof("enabling single-node mode")
		raftConfig.EnableSingleNode = true
		raftConfig.DisableBootstrapAfterElect = false
	}

	ip, err := b.config.GetIpByInterface()
	if err != nil {
		return err
	}

	// Setup Raft communication.
	raftAddr := &net.TCPAddr{IP: net.ParseIP(ip), Port: b.config.Ports["raft"]}
	transport, err := raft.NewTCPTransport(raftAddr.String(), raftAddr, 3, 10*time.Second, os.Stderr)
	if err != nil {
		return err
	}
	b.raftTransport = transport

	var log raft.LogStore
	var stable raft.StableStore
	var snap raft.SnapshotStore

	if b.config.DevMode {
		store := raft.NewInmemStore()
		b.raftInmem = store
		stable = store
		log = store
		snap = raft.NewDiscardSnapshotStore()
		b.raftPeers = &raft.StaticPeers{}
	} else {
		// Create peer storage.
		peerStore := raft.NewJSONPeers(b.config.ConfigPath, transport)
		b.raftPeers = peerStore

		var snapshots *raft.FileSnapshotStore
		// Create the snapshot store. This allows the Raft to truncate the log.
		snapshots, err = raft.NewFileSnapshotStore(b.config.ConfigPath, retainSnapshotCount, os.Stderr)
		if err != nil {
			return fmt.Errorf("file snapshot store: %s", err)
		}
		snap = snapshots

		var logStore *raftboltdb.BoltStore
		// Create the log store and stable store.
		logStore, err = raftboltdb.NewBoltStore(filepath.Join(b.config.ConfigPath, "raft.db"))
		if err != nil {
			return fmt.Errorf("new bolt store: %s", err)
		}
		b.raftStore = logStore
		log = logStore
		stable = logStore
	}

	go b.watchState()

	// Instantiate the Raft systems.
	ra, err := raft.NewRaft(raftConfig, b.engine, log, stable, snap, b.raftPeers, transport)
	if err != nil {
		return fmt.Errorf("new raft: %s", err)
	}
	b.raft = ra

	return nil
}

func (b *Balancer) watchState() {
	for {
		select {
		case rsp := <-b.engine.StateCh:
			// TODO: this doesn't need to run all the time, we can implement
			// some kind of throttling in the future waiting for a threashold of
			// messages before applying the messages.
			rsp <- b.handleStateChange()
		}
	}
}

func (b *Balancer) handleStateChange() error {
	if b.IsLeader() {
		b.provider.SyncVIPs(b.engine.State)
	} else {
		b.Lock()
		defer b.Unlock()
	}
	return b.engine.Ipvs.SyncState(b.engine.State)
}

func (b *Balancer) IsLeader() bool {
	return b.raft.State() == raft.Leader
}

func (b *Balancer) GetLeader() string {
	return b.raft.Leader()
}

// JoinPool joins the Fusis Serf cluster
func (b *Balancer) JoinPool() error {
	b.logger.Infof("Balancer: joining: %v", b.config.Join)

	_, err := b.serf.Join(b.config.Join, true)
	if err != nil {
		b.logger.Errorf("Balancer: error joining: %v", err)
		return err
	}

	return nil
}

func (b *Balancer) watchLeaderChanges() {
	b.logger.Infof("Watching to Leader changes")

	for {
		isLeader := <-b.raft.LeaderCh()
		b.Lock()
		if isLeader {
			b.flushVips()
			b.setVips()
		} else {
			b.flushVips()
		}
		b.Unlock()
	}
}

func (b *Balancer) handleEvents() {
	for {
		select {
		case e := <-b.eventCh:
			switch e.EventType() {
			case serf.EventMemberJoin:
				me := e.(serf.MemberEvent)
				b.handleMemberJoin(me)
			case serf.EventMemberFailed:
				memberEvent := e.(serf.MemberEvent)
				b.handleMemberLeave(memberEvent)
			case serf.EventMemberLeave:
				memberEvent := e.(serf.MemberEvent)
				b.handleMemberLeave(memberEvent)
			// case serf.EventQuery:
			// 	query := e.(*serf.Query)
			// 	b.handleQuery(query)
			default:
				b.logger.Warnf("Balancer: unhandled Serf Event: %#v", e)
			}
		}
	}
}

func (b *Balancer) setVips() {
	err := b.provider.SyncVIPs(b.engine.State)
	if err != nil {
		//TODO: Remove balancer from cluster when error occurs
		b.logger.Error(err)
	}
}

func (b *Balancer) flushVips() {
	if err := fusis_net.DelVips(b.config.Provider.Params["interface"]); err != nil {
		//TODO: Remove balancer from cluster when error occurs
		b.logger.Error(err)
	}
}

func (b *Balancer) handleMemberJoin(event serf.MemberEvent) {
	b.logger.Infof("handleMemberJoin: %s", event)

	if !b.IsLeader() {
		return
	}

	for _, m := range event.Members {
		if isBalancer(m) {
			b.addMemberToPool(m)
		}
	}
}

func (b *Balancer) addMemberToPool(m serf.Member) {
	remoteAddr := fmt.Sprintf("%s:%v", m.Addr.String(), m.Tags["raft-port"])

	b.logger.Infof("Adding Balancer to Pool", remoteAddr)
	f := b.raft.AddPeer(remoteAddr)
	if f.Error() != nil {
		b.logger.Errorf("node at %s joined failure. err: %s", remoteAddr, f.Error())
	}
}

func isBalancer(m serf.Member) bool {
	return m.Tags["role"] == "balancer"
}

func (b *Balancer) handleMemberLeave(memberEvent serf.MemberEvent) {
	b.logger.Infof("handleMemberLeave: %s", memberEvent)
	for _, m := range memberEvent.Members {
		if isBalancer(m) {
			b.handleBalancerLeave(m)
		} else {
			b.handleAgentLeave(m)
		}
	}
}

func (b *Balancer) handleBalancerLeave(m serf.Member) {
	b.logger.Info("Removing left balancer from raft")
	if !b.IsLeader() {
		b.logger.Info("Member is not leader")
		return
	}

	raftPort, err := strconv.Atoi(m.Tags["raft-port"])
	if err != nil {
		b.logger.Errorln("handle balancer leaver failed", err)
	}

	peer := &net.TCPAddr{IP: m.Addr, Port: raftPort}
	b.logger.Infof("Removing %v peer from raft", peer)

	future := b.raft.RemovePeer(peer.String())
	if err := future.Error(); err != nil && err != raft.ErrUnknownPeer {
		b.logger.Errorf("balancer: failed to remove raft peer '%v': %v", peer, err)
	} else if err == nil {
		b.logger.Infof("balancer: removed balancer '%s' as peer", m.Name)
	}
}

func (b *Balancer) Leave() {
	b.logger.Info("balancer: server starting leave")
	// s.left = true

	// Check the number of known peers
	numPeers, err := b.numOtherPeers()
	if err != nil {
		b.logger.Errorf("balancer: failed to check raft peers: %v", err)
		return
	}

	// If we are the current leader, and we have any other peers (cluster has multiple
	// servers), we should do a RemovePeer to safely reduce the quorum size. If we are
	// not the leader, then we should issue our leave intention and wait to be removed
	// for some sane period of time.
	isLeader := b.IsLeader()
	// if isLeader && numPeers > 0 {
	// 	future := b.raft.RemovePeer(b.raftTransport.LocalAddr())
	// 	if err := future.Error(); err != nil && err != raft.ErrUnknownPeer {
	// 		b.logger.Errorf("balancer: failed to remove ourself as raft peer: %v", err)
	// 	}
	// }

	// Leave the LAN pool
	if b.serf != nil {
		if err := b.serf.Leave(); err != nil {
			b.logger.Errorf("balancer: failed to leave LAN Serf cluster: %v", err)
		}
	}

	// If we were not leader, wait to be safely removed from the cluster.
	// We must wait to allow the raft replication to take place, otherwise
	// an immediate shutdown could cause a loss of quorum.
	if !isLeader {
		limit := time.Now().Add(raftRemoveGracePeriod)
		for numPeers > 0 && time.Now().Before(limit) {
			// Update the number of peers
			numPeers, err = b.numOtherPeers()
			if err != nil {
				b.logger.Errorf("balancer: failed to check raft peers: %v", err)
				break
			}

			// Avoid the sleep if we are done
			if numPeers == 0 {
				break
			}

			// Sleep a while and check again
			time.Sleep(50 * time.Millisecond)
		}
		if numPeers != 0 {
			b.logger.Warnln("balancer: failed to leave raft peer set gracefully, timeout")
		}
	}
}

// numOtherPeers is used to check on the number of known peers
// excluding the local node
func (b *Balancer) numOtherPeers() (int, error) {
	peers, err := b.raftPeers.Peers()
	if err != nil {
		return 0, err
	}
	otherPeers := raft.ExcludePeer(peers, b.raftTransport.LocalAddr())
	return len(otherPeers), nil
}

func (b *Balancer) Shutdown() {
	b.Leave()
	b.serf.Shutdown()

	future := b.raft.Shutdown()
	if err := future.Error(); err != nil {
		b.logger.Errorf("balancer: Error shutting down raft: %s", err)
	}

	if b.raftStore != nil {
		b.raftStore.Close()
	}

	b.raftPeers.SetPeers(nil)
}

func (b *Balancer) handleAgentLeave(m serf.Member) {
	dst, err := b.GetDestination(m.Name)
	if err != nil {
		b.logger.Errorln("handleAgenteLeave failed", err)
		return
	}

	b.DeleteDestination(dst)
}

func (b *Balancer) collectStats() {

	interval := b.config.Stats.Interval

	if interval > 0 {
		ticker := time.NewTicker(time.Second * time.Duration(interval))
		for tick := range ticker.C {
			b.engine.CollectStats(tick)
		}
	}
}

func readPeersJSON(path string) ([]string, error) {
	b, err := ioutil.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	if len(b) == 0 {
		return nil, nil
	}

	var peers []string
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(&peers); err != nil {
		return nil, err
	}

	return peers, nil
}
