package tcp

import (
	"context"
	"github.com/cloudwego/netpoll"
	"github.com/dobyte/due/v2/errors"
	"github.com/dobyte/due/v2/log"
	"github.com/dobyte/due/v2/network"
	"github.com/dobyte/due/v2/packet"
	"github.com/dobyte/due/v2/utils/xcall"
	"github.com/dobyte/due/v2/utils/xnet"
	"github.com/dobyte/due/v2/utils/xtime"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

type clientConn struct {
	id                int64              // 连接ID
	uid               int64              // 用户ID
	conn              netpoll.Connection // TCP源连接
	state             int32              // 连接状态
	client            *client            // 客户端
	lastHeartbeatTime int64              // 上次心跳时间
	rw                sync.RWMutex       // 读写锁
	chWrite           chan chWrite       // 写入队列
	done              chan struct{}      // 写入完成信号
	//close             chan struct{}      // 关闭信号
	ctx    context.Context
	cancel context.CancelFunc
}

var _ network.Conn = &clientConn{}

func newClientConn(client *client, id int64, conn netpoll.Connection) network.Conn {
	c := &clientConn{}
	c.id = id
	c.conn = conn
	c.state = int32(network.ConnOpened)
	c.client = client
	c.lastHeartbeatTime = xtime.Now().Unix()
	c.ctx, c.cancel = context.WithCancel(context.Background())

	xcall.Go(c.read)

	xcall.Go(c.heartbeat)

	if c.client.connectHandler != nil {
		c.client.connectHandler(c)
	}

	return c
}

// ID 获取连接ID
func (c *clientConn) ID() int64 {
	return c.id
}

// UID 获取用户ID
func (c *clientConn) UID() int64 {
	return atomic.LoadInt64(&c.uid)
}

// Bind 绑定用户ID
func (c *clientConn) Bind(uid int64) {
	atomic.StoreInt64(&c.uid, uid)
}

// Unbind 解绑用户ID
func (c *clientConn) Unbind() {
	atomic.StoreInt64(&c.uid, 0)
}

// Send 发送消息（同步）
func (c *clientConn) Send(msg []byte) error {
	if err := c.checkState(); err != nil {
		return err
	}

	conn := c.conn

	if conn == nil {
		return errors.ErrConnectionClosed
	}

	return write(conn.Writer(), msg)
}

// Push 发送消息（异步）
func (c *clientConn) Push(msg []byte) (err error) {
	return c.Send(msg)
	//if err = c.checkState(); err != nil {
	//	return
	//}
	//
	//c.rw.RLock()
	//c.chWrite <- chWrite{typ: dataPacket, msg: msg}
	//c.rw.RUnlock()
	//
	//return
}

// State 获取连接状态
func (c *clientConn) State() network.ConnState {
	return network.ConnState(atomic.LoadInt32(&c.state))
}

// Close 关闭连接
func (c *clientConn) Close(isForce ...bool) error {
	return c.close()
	//if len(isForce) > 0 && isForce[0] {
	//	return c.forceClose()
	//} else {
	//	return c.graceClose()
	//}
}

// LocalIP 获取本地IP
func (c *clientConn) LocalIP() (string, error) {
	addr, err := c.LocalAddr()
	if err != nil {
		return "", err
	}

	return xnet.ExtractIP(addr)
}

// LocalAddr 获取本地地址
func (c *clientConn) LocalAddr() (net.Addr, error) {
	if err := c.checkState(); err != nil {
		return nil, err
	}

	conn := c.conn

	if conn == nil {
		return nil, errors.ErrConnectionClosed
	}

	return conn.LocalAddr(), nil
}

// RemoteIP 获取远端IP
func (c *clientConn) RemoteIP() (string, error) {
	addr, err := c.RemoteAddr()
	if err != nil {
		return "", err
	}

	return xnet.ExtractIP(addr)
}

// RemoteAddr 获取远端地址
func (c *clientConn) RemoteAddr() (net.Addr, error) {
	if err := c.checkState(); err != nil {
		return nil, err
	}

	conn := c.conn

	if conn == nil {
		return nil, errors.ErrConnectionClosed
	}

	return conn.RemoteAddr(), nil
}

// 检测连接状态
func (c *clientConn) checkState() error {
	switch network.ConnState(atomic.LoadInt32(&c.state)) {
	case network.ConnHanged:
		return errors.ErrConnectionHanged
	case network.ConnClosed:
		return errors.ErrConnectionClosed
	default:
		return nil
	}
}

// 关闭连接
func (c *clientConn) close() error {
	if !atomic.CompareAndSwapInt32(&c.state, int32(network.ConnOpened), int32(network.ConnClosed)) {
		return errors.ErrConnectionClosed
	}

	c.cancel()
	err := c.conn.Close()
	c.conn = nil

	if c.client.disconnectHandler != nil {
		c.client.disconnectHandler(c)
	}

	return err
}

// 读取消息
func (c *clientConn) read() {
	conn := c.conn
	reader := conn.Reader()

	for {
		select {
		case <-c.ctx.Done():
			return
		default:
			// block reading messages from the server
			msg, err := packet.ReadMessage(reader)
			if err != nil {
				if conn.IsActive() {
					continue
				}

				_ = c.close()
				return
			}

			// ignore empty packet
			if len(msg) == 0 {
				continue
			}

			// check heartbeat packet
			isHeartbeat, err := packet.CheckHeartbeat(msg)
			if err != nil {
				log.Errorf("check heartbeat message error: %v", err)
				return
			}

			if c.client.opts.heartbeatInterval > 0 {
				atomic.StoreInt64(&c.lastHeartbeatTime, xtime.Now().Unix())
			}

			if isHeartbeat {
				continue
			}

			if c.client.receiveHandler != nil {
				c.client.receiveHandler(c, msg)
			}
		}
	}
}

// 心跳检测
func (c *clientConn) heartbeat() {
	if c.client.opts.heartbeatInterval <= 0 {
		return
	}

	writer := c.conn.Writer()
	ticker := time.NewTicker(c.client.opts.heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			deadline := xtime.Now().Add(-2 * c.client.opts.heartbeatInterval).Unix()

			if atomic.LoadInt64(&c.lastHeartbeatTime) < deadline {
				log.Warnf("connection heartbeat timeout")
				_ = c.close()
				return
			} else {
				heartbeat, err := packet.PackHeartbeat()
				if err != nil {
					log.Warnf("pack heartbeat message failed: %v", err)
					continue
				}

				// send heartbeat packet
				if err = write(writer, heartbeat); err != nil {
					log.Warnf("send heartbeat message failed: %v", err)
				}
			}
		}
	}
}

// 是否已关闭
func (c *clientConn) isClosed() bool {
	return network.ConnState(atomic.LoadInt32(&c.state)) == network.ConnClosed
}