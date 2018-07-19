// Copyright 2018 The go-hpb Authors
// This file is part of the go-hpb.
//
// The go-hpb is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-hpb is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-hpb. If not, see <http://www.gnu.org/licenses/>.

package p2p

import (
	"sync"
	"errors"
	"math/big"
	"math/rand"
	"github.com/hpb-project/go-hpb/common"
	"github.com/hpb-project/go-hpb/common/log"
	"github.com/hpb-project/go-hpb/config"
	"github.com/hpb-project/go-hpb/network/rpc"
	"sync/atomic"
	"github.com/hpb-project/go-hpb/network/p2p/discover"
	"net"
	"github.com/hpb-project/go-hpb/network/p2p/iperf"
	"time"
	"fmt"
	"path/filepath"
)

var (
	errClosed            = errors.New("peer set is closed")
	errAlreadyRegistered = errors.New("peer is already registered")
	errNotRegistered     = errors.New("peer is not registered")
	errIncomplete        = errors.New("PeerManager is incomplete creation")
)

const (
	maxKnownTxs      = 1000000 // Maximum transactions hashes to keep in the known list (prevent DOS) //for testnet
	maxKnownBlocks   = 100000  // Maximum block hashes to keep in the known list (prevent DOS)  //for testnet
)

type PeerManager struct {
	peers  map[string]*Peer
	boots  map[string]*Peer
	lock   sync.RWMutex
	closed bool

	rpcmgr *RpcMgr
	server *Server
	hpbpro *HpbProto
}

var INSTANCE = atomic.Value{}

func PeerMgrInst() *PeerManager {
	if INSTANCE.Load() == nil {
		pm :=&PeerManager{
			peers:  make(map[string]*Peer),
			boots:  make(map[string]*Peer),
			server: &Server{},
			rpcmgr: &RpcMgr{},
			hpbpro: NewProtos(),
		}
		INSTANCE.Store(pm)
	}

	return INSTANCE.Load().(*PeerManager)
}

func (prm *PeerManager)Start() error {

	config, err :=config.GetHpbConfigInstance()
	if err != nil {
		log.Error("Peer manager get config error","error",err)
		return err
	}

	prm.server.Config = Config{
		NAT:        config.Network.NAT,
		Name:       config.Network.Name,
		PrivateKey: config.Node.PrivateKey,
		NetworkId:  config.Node.NetworkId,
		DefaultAddr:config.Node.DefaultAddress,
		ListenAddr: config.Network.ListenAddr,

		NetRestrict:    config.Network.NetRestrict,
		NodeDatabase:   config.Network.NodeDatabase,
		BootstrapNodes: config.Network.BootstrapNodes,
		EnableMsgEvents:config.Network.EnableMsgEvents,

		Protocols: prm.hpbpro.Protocols(),
	}

	prm.hpbpro.networkId   = prm.server.NetworkId
	prm.hpbpro.regMsgProcess(ReqNodesMsg,HandleReqNodesMsg)
	prm.hpbpro.regMsgProcess(ResNodesMsg,HandleResNodesMsg)
	copy(prm.server.Protocols, prm.hpbpro.Protocols())


	prm.server.localType = discover.PreNode
	if config.Network.RoleType == "bootnode" {
		prm.server.localType = discover.BootNode

		//input cid&hib from json
		filename := filepath.Join(config.Node.DataDir, bindInfoFileName)
		log.Debug("bootnode load bindings","filename",filename)
		parseBindInfo(filename)

	}


	log.Info("Manager start server para","NodeType",prm.server.localType.ToString())

	if err := prm.server.Start(); err != nil {
		log.Error("Hpb protocol","error",err)
		return err
	}

	// for-test
	log.Info("para from config","IpcEndpoint",config.Network.IpcEndpoint,"HttpEndpoint",config.Network.HttpEndpoint,"WsEndpoint",config.Network.WsEndpoint)
	ipcEndpoint:=  config.Network.IpcEndpoint
	httpEndpoint:= config.Network.HttpEndpoint
	wsEndpoint  := config.Network.WsEndpoint

	prm.rpcmgr    = &RpcMgr{
		ipcEndpoint:  ipcEndpoint,
		httpEndpoint: httpEndpoint,
		wsEndpoint:   wsEndpoint,

		httpCors:     config.Network.HTTPCors,
		httpModules:  config.Network.HTTPModules,

		wsOrigins:    config.Network.WSOrigins,
		wsModules:    config.Network.WSModules,
		wsExposeAll:  config.Network.WSExposeAll,
	}
	prm.rpcmgr.startRPC(config.Node.RpcAPIs)


	add,err:=net.ResolveUDPAddr("udp",prm.server.ListenAddr)
	add.Port = add.Port+100
	log.Info("Iperf server start", "port",add.Port)
	go iperf.StartSever(add.Port)
	if prm.server.localType != discover.BootNode {
		go prm.randomTestBW()
	}

	return nil
}



func (prm *PeerManager)Stop(){

	prm.Close()
	prm.rpcmgr.stopRPC()

	prm.server.Stop()
	prm.server = nil

	iperf.KillSever()

}

func (prm *PeerManager) randomTestBW() {
	rd :=rand.Intn(30)
	timeout := time.NewTimer(time.Second*time.Duration(30+rd))
	defer timeout.Stop()

	for {
		//1 start to test
		//log.Info("waiting start test")
		select {
		case <-timeout.C:
			rd :=rand.Intn(6)
			timeout.Reset(time.Second*time.Duration(60*10+rd))
		}

		//2 to test
		for _, p := range prm.peers {
			if p.remoteType == discover.BootNode {
				continue
			}

			p.log.Info("start testing","remoteType",p.remoteType.ToString())
			if err:= p.testBandwidth();err != nil{
				p.log.Error("random test bandwidth","error",err)
			}else {
				break
			}
		}

	}

	return
}

func (prm *PeerManager)P2pSvr() *Server {
	return prm.server
}

func (prm *PeerManager)IpcHandle() *rpc.Server {
	return prm.rpcmgr.inprocHandler
}

// Register injects a new peer into the working set, or returns an error if the
// peer is already known.
func (prm *PeerManager) Register(p *Peer) error {
	prm.lock.Lock()
	defer prm.lock.Unlock()

	if prm.closed {
		return errClosed
	}
	if p.remoteType == discover.BootNode{
		if _, ok := prm.boots[p.id]; !ok {
			prm.boots[p.id] = p
			log.Info("Peer with bootnode is listed.")
		}
		return nil
	}

	if _, ok := prm.peers[p.id]; ok {
		return errAlreadyRegistered
	}
	prm.peers[p.id] = p
	return nil
}

// Unregister removes a remote peer from the active set, disabling any further
// actions to/from that particular entity.
func (prm *PeerManager) Unregister(id string) error {
	prm.lock.Lock()
	defer prm.lock.Unlock()

	if _, ok := prm.peers[id]; ok {
		delete(prm.peers, id)
	}

	if _, ok := prm.boots[id]; ok {
		delete(prm.boots, id)
	}

	return nil
}


// Peer retrieves the registered peer with the given id.
func (prm *PeerManager) Peer(id string) *Peer {
	prm.lock.RLock()
	defer prm.lock.RUnlock()

	return prm.peers[id]
}

func (prm *PeerManager) PeersAll() []*Peer {
	prm.lock.RLock()
	defer prm.lock.RUnlock()

	list := make([]*Peer, 0, len(prm.peers))
	for _, p := range prm.peers {
		list = append(list, p)
	}
	return list
}

// Len returns if the current number of peers in the set.
func (prm *PeerManager) Len() int {
	prm.lock.RLock()
	defer prm.lock.RUnlock()

	return len(prm.peers)
}

// PeersWithoutBlock retrieves a list of peers that do not have a given block in
// their set of known hashes.
func (prm *PeerManager) PeersWithoutBlock(hash common.Hash) []*Peer {
	prm.lock.RLock()
	defer prm.lock.RUnlock()

	list := make([]*Peer, 0, len(prm.peers))
	for _, p := range prm.peers {
		if !p.knownBlocks.Has(hash) {
			list = append(list, p)
		}
	}
	return list
}

// PeersWithoutTx retrieves a list of peers that do not have a given transaction
// in their set of known hashes.
func (prm *PeerManager) PeersWithoutTx(hash common.Hash) []*Peer {
	prm.lock.RLock()
	defer prm.lock.RUnlock()

	list := make([]*Peer, 0, len(prm.peers))
	for _, p := range prm.peers {
		if !p.knownTxs.Has(hash) {
			list = append(list, p)
		}
	}
	return list
}

// BestPeer retrieves the known peer with the currently highest total difficulty.
func (prm *PeerManager) BestPeer() *Peer {
	prm.lock.RLock()
	defer prm.lock.RUnlock()

	var (
		bestPeer *Peer
		bestTd   *big.Int
	)
	for _, p := range prm.peers {
		if _, td := p.Head(); bestPeer == nil || td.Cmp(bestTd) > 0 {
			bestPeer, bestTd = p, td
		}
	}
	return bestPeer
}

// Close disconnects all peers.
// No new peers can be registered after Close has returned.
func (prm *PeerManager) Close() {
	prm.lock.Lock()
	defer prm.lock.Unlock()

	for _, p := range prm.peers {
		p.Disconnect(DiscQuitting)
	}
	prm.closed = true
}

func (prm *PeerManager) Protocol() []Protocol {
	return prm.hpbpro.protos
}
////////////////////////////////////////////////////////////////////

type PeerInfo struct {
	ID      string   `json:"id"`   // Unique node identifier (also the encryption key)
	Name    string   `json:"name"` // Name of the node, including client type, version, OS, custom data
	Remote  string   `json:"remote"` //Remote node type
	Cap     string   `json:"cap"` // Sum-protocols advertised by this particular peer
	Network struct {
		Local  string `json:"local"`  // Local endpoint of the TCP data connection
		Remote string `json:"remote"` // Remote endpoint of the TCP data connection
	} `json:"network"`
	HPB interface{} `json:"hpb"` // Sub-protocol specific metadata fields
}

type HpbInfo struct {
	Version  uint     `json:"version"`     // Hpb protocol version negotiated
	TD       *big.Int `json:"handshakeTD"` // Total difficulty of the peer's blockchain
	Head     string   `json:"handshakeHD"` // SHA3 hash of the peer's best owned block
}

func (prm *PeerManager) PeersInfo() []*PeerInfo {
	prm.lock.RLock()
	defer prm.lock.RUnlock()


	allinfos := make([]*PeerInfo, 0, len(prm.boots)+len(prm.peers))
	for _, p := range prm.boots {
		info := &PeerInfo{
			ID:        p.ID().TerminalString(),
			Name:      p.Name(),
			Remote:    p.remoteType.ToString(),
			Cap:       p.Caps()[0].String(),
			HPB:       "",
		}
		info.Network.Local  = p.LocalAddr().String()
		info.Network.Remote = p.RemoteAddr().String()
		allinfos = append(allinfos, info)
	}

	peerinfos := make([]*PeerInfo, 0, len(prm.peers))
	for _, p := range prm.peers {
		hash, td := p.Head()
		info := &PeerInfo{
			ID:        p.ID().TerminalString(),
			Name:      p.Name(),
			Remote:    p.remoteType.ToString(),
			Cap:       p.Caps()[0].String(),
			HPB:       &HpbInfo{
				Version:    p.version,
				TD: td,
				Head: hash.Hex(),
			},
		}
		info.Network.Local  = p.LocalAddr().String()
		info.Network.Remote = p.RemoteAddr().String()
		peerinfos = append(peerinfos, info)
	}

	for i := 0; i < len(peerinfos); i++ {
		for j := i + 1; j < len(peerinfos); j++ {
			if peerinfos[i].ID > peerinfos[j].ID {
				peerinfos[i], peerinfos[j] = peerinfos[j], peerinfos[i]
			}
		}
	}
	allinfos = append(allinfos, peerinfos...)

	return allinfos
}


type NodeInfo struct {
	ID    string `json:"id"`    // Unique node identifier (also the encryption key)
	Name  string `json:"name"`  // Name of the node, including client type, version, OS, custom data
	Local string `json:"local"` // Local node type
	IP    string `json:"ip"`    // IP address of the node
	Ports struct {
		UDP int `json:"udp"`   // UDP listening port for discovery protocol
		TCP  int `json:"tcp"`  // TCP listening port for RLPx
	} `json:"ports"`
	ListenAddr string `json:"listenAddr"`
}

func (prm *PeerManager) NodeInfo() *NodeInfo {
	node := prm.server.Self()

	info := &NodeInfo{
		Name:       prm.server.Name,
		Local:      prm.server.localType.ToString(),
		ID:         node.ID.String(),
		IP:         node.IP.String(),
		ListenAddr: prm.server.ListenAddr,
	}
	info.Ports.UDP = int(node.UDP)
	info.Ports.TCP = int(node.TCP)

	return info
}

////////////////////////////////////////////////////////////////////
func (prm *PeerManager) RegMsgProcess(msg uint64,cb MsgProcessCB) {
	prm.hpbpro.regMsgProcess(msg,cb)
	return
}

func (prm *PeerManager) RegChanStatus(cb ChanStatusCB) {
	prm.hpbpro.regChanStatus(cb)
	log.Info("ChanStatus has been register")
	return
}


func (prm *PeerManager) RegOnAddPeer(cb OnAddPeerCB) {
	prm.hpbpro.regOnAddPeer(cb)
	log.Info("OnAddPeer has been register")
	return
}

func (prm *PeerManager) RegOnDropPeer(cb OnDropPeerCB) {
	prm.hpbpro.regOnDropPeer(cb)
	log.Debug("OnDropPeer has been register")
	return
}

////////////////////////////////////////////////////////////////////
const  bindInfoFileName  = "binding.json"
type BindInfo struct {
	CID    string     `json:"cid"`
	HIB    string     `json:"hib"`
	ADR    string     `json:"address"`
	AUT    string     `json:"-"`
}
func parseBindInfo(filename string) error{

	// Load the nodes from the config file.
	var bindings [] BindInfo

	if err := common.LoadJSON(filename, &bindings); err != nil {
		log.Warn(fmt.Sprintf("Can't load node file %s: %v", filename, err))
		return nil
	}

	log.Debug("parse binding","information",bindings)

	return nil
}



