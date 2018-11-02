package cluster

import (
	"bytes"
	"strings"
	"sync/atomic"

	"overlord/lib/conv"
	"overlord/proto"
	"overlord/proto/redis"

	"github.com/pkg/errors"
)

const (
	respRedirect = '-'
)

var (
	askBytes   = []byte("ASK")
	movedBytes = []byte("MOVED")

	askingResp = []byte("*1\r\n$6\r\nASKING\r\n")
)

type nodeConn struct {
	c  *cluster
	nc proto.NodeConn

	sb strings.Builder

	state int32
}

func newNodeConn(c *cluster, addr string) proto.NodeConn {
	return &nodeConn{
		c:  c,
		nc: redis.NewNodeConn(c.name, addr, c.dto, c.rto, c.wto),
	}
}

func (nc *nodeConn) Write(m *proto.Message) (err error) {
	if err = nc.nc.Write(m); err != nil {
		err = errors.WithStack(err)
	}
	return
}

func (nc *nodeConn) Flush() error {
	return nc.nc.Flush()
}

func (nc *nodeConn) Read(m *proto.Message) (err error) {
	if err = nc.nc.Read(m); err != nil {
		err = errors.WithStack(err)
		return
	}
	req := m.Request().(*redis.Request)
	// check request
	if !req.IsSupport() || req.IsCtl() {
		return
	}
	reply := req.Reply()
	if reply.Type() != respRedirect {
		return
	}
	data := reply.Data()
	if !bytes.HasPrefix(data, askBytes) && !bytes.HasPrefix(data, movedBytes) {
		return
	}
	addrBs, _, isAsk, _ := parseRedirect(data)
	nc.sb.Reset()
	nc.sb.Write(addrBs)
	addr := nc.sb.String()
	// redirect process
	err = nc.redirectProcess(m, req, addr, isAsk)
	return
}

func (nc *nodeConn) redirectProcess(m *proto.Message, req *redis.Request, addr string, isAsk bool) (err error) {
	rdt := nc.c.getRedirectNodeConn(addr)
	rdt.lock.Lock()
	defer rdt.lock.Unlock()
	if rdt.nc == nil {
		err = errors.WithStack(ErrClusterClosed)
		return
	}
	rnc := rdt.nc
	if isAsk {
		if err = rnc.Bw().Write(askingResp); err != nil {
			err = errors.WithStack(err)
			return
		}
	}
	if err = req.RESP().Encode(rnc.Bw()); err != nil {
		err = errors.WithStack(err)
		return
	}
	if err = rnc.Bw().Flush(); err != nil {
		err = errors.WithStack(err)
		return
	}
	if err = rnc.Read(m); err != nil {
		err = errors.WithStack(err)
	}
	nc.c.closeRedirectNodeConn(addr, isAsk)
	return
}

func (nc *nodeConn) Close() (err error) {
	if atomic.CompareAndSwapInt32(&nc.state, opening, closed) {
		return nc.nc.Close()
	}
	return
}

func parseRedirect(data []byte) (addr []byte, slot int, isAsk bool, err error) {
	fields := bytes.Fields(data)
	if len(fields) != 3 {
		return
	}
	si, err := conv.Btoi(fields[1])
	if err != nil {
		return
	}
	addr = fields[2]
	slot = int(si)
	isAsk = bytes.Equal(askBytes, fields[0])
	return
}
