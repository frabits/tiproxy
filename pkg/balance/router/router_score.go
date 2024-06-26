// Copyright 2023 PingCAP, Inc.
// SPDX-License-Identifier: Apache-2.0

package router

import (
	"context"
	"sync"
	"time"

	glist "github.com/bahlo/generic-list-go"
	"github.com/pingcap/tiproxy/lib/util/errors"
	"github.com/pingcap/tiproxy/lib/util/waitgroup"
	"github.com/pingcap/tiproxy/pkg/balance/observer"
	"github.com/pingcap/tiproxy/pkg/util/monotime"
	"go.uber.org/zap"
)

const (
	_routerKey = "__tiproxy_router"
)

var _ Router = &ScoreBasedRouter{}

// ScoreBasedRouter is an implementation of Router interface.
// It routes a connection based on score.
type ScoreBasedRouter struct {
	sync.Mutex
	logger     *zap.Logger
	observer   observer.BackendObserver
	healthCh   <-chan observer.HealthResult
	cancelFunc context.CancelFunc
	wg         waitgroup.WaitGroup
	// A list of *backendWrapper. The backends are in descending order of scores.
	backends     *glist.List[*backendWrapper]
	observeError error
	// Only store the version of a random backend, so the client may see a wrong version when backends are upgrading.
	serverVersion string
}

// NewScoreBasedRouter creates a ScoreBasedRouter.
func NewScoreBasedRouter(logger *zap.Logger) *ScoreBasedRouter {
	return &ScoreBasedRouter{
		logger:   logger,
		backends: glist.New[*backendWrapper](),
	}
}

func (r *ScoreBasedRouter) Init(ctx context.Context, ob observer.BackendObserver) {
	r.observer = ob
	r.healthCh = r.observer.Subscribe("score_based_router")
	childCtx, cancelFunc := context.WithCancel(ctx)
	r.cancelFunc = cancelFunc
	// Failing to rebalance backends may cause even more serious problems than TiProxy reboot, so we don't recover panics.
	r.wg.Run(func() {
		r.rebalanceLoop(childCtx)
	})
}

// GetBackendSelector implements Router.GetBackendSelector interface.
func (router *ScoreBasedRouter) GetBackendSelector() BackendSelector {
	return BackendSelector{
		routeOnce: router.routeOnce,
		onCreate:  router.onCreateConn,
	}
}

func (router *ScoreBasedRouter) getConnWrapper(conn RedirectableConn) *glist.Element[*connWrapper] {
	return conn.Value(_routerKey).(*glist.Element[*connWrapper])
}

func (router *ScoreBasedRouter) setConnWrapper(conn RedirectableConn, ce *glist.Element[*connWrapper]) {
	conn.SetValue(_routerKey, ce)
}

func (router *ScoreBasedRouter) routeOnce(excluded []BackendInst) (BackendInst, error) {
	router.Lock()
	defer router.Unlock()
	if router.observeError != nil {
		return nil, router.observeError
	}
	for be := router.backends.Back(); be != nil; be = be.Prev() {
		backend := be.Value
		// These backends may be recycled, so we should not connect to them again.
		switch backend.Status() {
		case observer.StatusCannotConnect, observer.StatusSchemaOutdated:
			continue
		}
		found := false
		for _, ex := range excluded {
			if ex.Addr() == backend.Addr() {
				found = true
				break
			}
		}
		if !found {
			backend.connScore++
			router.adjustBackendList(be, false)
			return backend, nil
		}
	}
	// No available backends, maybe the health check result is outdated during rolling restart.
	// Refresh the backends asynchronously in this case.
	if router.observer != nil {
		router.observer.Refresh()
	}
	return nil, ErrNoBackend
}

func (router *ScoreBasedRouter) onCreateConn(backendInst BackendInst, conn RedirectableConn, succeed bool) {
	router.Lock()
	defer router.Unlock()
	be := router.ensureBackend(backendInst.Addr(), true)
	backend := be.Value
	if succeed {
		connWrapper := &connWrapper{
			RedirectableConn: conn,
			phase:            phaseNotRedirected,
		}
		router.addConn(be, connWrapper)
		conn.SetEventReceiver(router)
	} else {
		backend.connScore--
		router.adjustBackendList(be, true)
	}
}

func (router *ScoreBasedRouter) removeConn(be *glist.Element[*backendWrapper], ce *glist.Element[*connWrapper]) {
	backend := be.Value
	backend.connList.Remove(ce)
	setBackendConnMetrics(backend.addr, backend.connList.Len())
	router.adjustBackendList(be, true)
}

func (router *ScoreBasedRouter) addConn(be *glist.Element[*backendWrapper], conn *connWrapper) {
	backend := be.Value
	ce := backend.connList.PushBack(conn)
	setBackendConnMetrics(backend.addr, backend.connList.Len())
	router.setConnWrapper(conn, ce)
	router.adjustBackendList(be, false)
}

// adjustBackendList moves `be` after the score of `be` changes to keep the list ordered.
func (router *ScoreBasedRouter) adjustBackendList(be *glist.Element[*backendWrapper], removeEmpty bool) {
	if removeEmpty && router.removeBackendIfEmpty(be) {
		return
	}

	backend := be.Value
	curScore := backend.score()
	var mark *glist.Element[*backendWrapper]
	for ele := be.Prev(); ele != nil; ele = ele.Prev() {
		b := ele.Value
		if b.score() >= curScore {
			break
		}
		mark = ele
	}
	if mark != nil {
		router.backends.MoveBefore(be, mark)
		return
	}
	for ele := be.Next(); ele != nil; ele = ele.Next() {
		b := ele.Value
		if b.score() <= curScore {
			break
		}
		mark = ele
	}
	if mark != nil {
		router.backends.MoveAfter(be, mark)
	}
}

// RefreshBackend implements Router.GetBackendSelector interface.
func (router *ScoreBasedRouter) RefreshBackend() {
	router.observer.Refresh()
}

// RedirectConnections implements Router.RedirectConnections interface.
// It redirects all connections compulsively. It's only used for testing.
func (router *ScoreBasedRouter) RedirectConnections() error {
	router.Lock()
	defer router.Unlock()
	for be := router.backends.Front(); be != nil; be = be.Next() {
		backend := be.Value
		for ce := backend.connList.Front(); ce != nil; ce = ce.Next() {
			// This is only for test, so we allow it to reconnect to the same backend.
			connWrapper := ce.Value
			if connWrapper.phase != phaseRedirectNotify {
				connWrapper.phase = phaseRedirectNotify
				// we dont care the results
				_ = connWrapper.Redirect(backend)
				connWrapper.redirectingBackend = backend
			}
		}
	}
	return nil
}

// forward is a hint to speed up searching.
func (router *ScoreBasedRouter) lookupBackend(addr string, forward bool) *glist.Element[*backendWrapper] {
	if forward {
		for be := router.backends.Front(); be != nil; be = be.Next() {
			backend := be.Value
			if backend.addr == addr {
				return be
			}
		}
	} else {
		for be := router.backends.Back(); be != nil; be = be.Prev() {
			backend := be.Value
			if backend.addr == addr {
				return be
			}
		}
	}
	return nil
}

func (router *ScoreBasedRouter) ensureBackend(addr string, forward bool) *glist.Element[*backendWrapper] {
	be := router.lookupBackend(addr, forward)
	if be == nil {
		// The backend should always exist if it will be needed. Add a warning and add it back.
		router.logger.Warn("backend is not found in the router", zap.String("backend_addr", addr), zap.Stack("stack"))
		backend := &backendWrapper{
			addr:     addr,
			connList: glist.New[*connWrapper](),
		}
		backend.setHealth(observer.BackendHealth{
			Status: observer.StatusCannotConnect,
		})
		be = router.backends.PushFront(backend)
		router.adjustBackendList(be, false)
	}
	return be
}

// OnRedirectSucceed implements ConnEventReceiver.OnRedirectSucceed interface.
func (router *ScoreBasedRouter) OnRedirectSucceed(from, to string, conn RedirectableConn) error {
	router.onRedirectFinished(from, to, conn, true)
	return nil
}

// OnRedirectFail implements ConnEventReceiver.OnRedirectFail interface.
func (router *ScoreBasedRouter) OnRedirectFail(from, to string, conn RedirectableConn) error {
	router.onRedirectFinished(from, to, conn, false)
	return nil
}

func (router *ScoreBasedRouter) onRedirectFinished(from, to string, conn RedirectableConn, succeed bool) {
	router.Lock()
	defer router.Unlock()
	fromBe := router.ensureBackend(from, true)
	toBe := router.ensureBackend(to, false)
	connWrapper := router.getConnWrapper(conn).Value
	if succeed {
		router.removeConn(fromBe, router.getConnWrapper(conn))
		router.addConn(toBe, connWrapper)
		connWrapper.phase = phaseRedirectEnd
	} else {
		fromBe.Value.connScore++
		router.adjustBackendList(fromBe, false)
		toBe.Value.connScore--
		router.adjustBackendList(toBe, true)
		connWrapper.phase = phaseRedirectFail
	}
	connWrapper.redirectingBackend = nil
	addMigrateMetrics(from, to, succeed, connWrapper.lastRedirect)
}

// OnConnClosed implements ConnEventReceiver.OnConnClosed interface.
func (router *ScoreBasedRouter) OnConnClosed(addr string, conn RedirectableConn) error {
	router.Lock()
	defer router.Unlock()
	be := router.ensureBackend(addr, true)
	connWrapper := router.getConnWrapper(conn)
	redirectingBackend := connWrapper.Value.redirectingBackend
	// If this connection is redirecting, decrease the score of the target backend.
	if redirectingBackend != nil {
		redirectingBackend.connScore--
		connWrapper.Value.redirectingBackend = nil
		if redirectingBe := router.lookupBackend(redirectingBackend.addr, true); redirectingBe != nil {
			router.adjustBackendList(redirectingBe, true)
		}
	} else {
		be.Value.connScore--
	}
	router.removeConn(be, connWrapper)
	return nil
}

func (router *ScoreBasedRouter) updateBackendHealth(healthResults observer.HealthResult) {
	router.Lock()
	defer router.Unlock()
	router.observeError = healthResults.Error()
	if router.observeError != nil {
		return
	}

	// `backends` contain all the backends, not only the updated ones.
	backends := healthResults.Backends()
	// If some backends are removed from the list, add them to `backends`.
	for be := router.backends.Front(); be != nil; be = be.Next() {
		addr := be.Value.addr
		if _, ok := backends[addr]; !ok {
			backends[addr] = &observer.BackendHealth{
				Status:  observer.StatusCannotConnect,
				PingErr: errors.New("removed from backend list"),
			}
		}
	}
	var serverVersion string
	for addr, health := range backends {
		be := router.lookupBackend(addr, true)
		if be == nil && health.Status != observer.StatusCannotConnect {
			router.logger.Info("update backend", zap.String("backend_addr", addr),
				zap.String("prev", "none"), zap.String("cur", health.String()))
			backend := &backendWrapper{
				addr:     addr,
				connList: glist.New[*connWrapper](),
			}
			backend.setHealth(*health)
			serverVersion = health.ServerVersion
			be = router.backends.PushBack(backend)
			router.adjustBackendList(be, false)
		} else if be != nil {
			backend := be.Value
			if !backend.Equals(*health) {
				router.logger.Info("update backend", zap.String("backend_addr", addr),
					zap.String("prev", backend.String()), zap.String("cur", health.String()))
				backend.setHealth(*health)
				router.adjustBackendList(be, true)
				if health.Status != observer.StatusCannotConnect {
					serverVersion = health.ServerVersion
				}
			}
		}
	}
	if len(serverVersion) > 0 {
		router.serverVersion = serverVersion
	}
}

func (router *ScoreBasedRouter) rebalanceLoop(ctx context.Context) {
	ticker := time.NewTicker(rebalanceInterval)
	for {
		select {
		case <-ctx.Done():
			ticker.Stop()
			return
		case healthResults := <-router.healthCh:
			router.updateBackendHealth(healthResults)
		case <-ticker.C:
			router.rebalance(rebalanceConnsPerLoop)
		}
	}
}

func (router *ScoreBasedRouter) rebalance(maxNum int) {
	curTime := monotime.Now()
	router.Lock()
	defer router.Unlock()
	for i := 0; i < maxNum; i++ {
		var busiestEle *glist.Element[*backendWrapper]
		for be := router.backends.Front(); be != nil; be = be.Next() {
			backend := be.Value
			if backend.connList.Len() > 0 {
				busiestEle = be
				break
			}
		}
		if busiestEle == nil {
			break
		}
		busiestBackend := busiestEle.Value
		idlestEle := router.backends.Back()
		idlestBackend := idlestEle.Value
		if float64(busiestBackend.score())/float64(idlestBackend.score()+1) < rebalanceMaxScoreRatio {
			break
		}
		var ce *glist.Element[*connWrapper]
		for ele := busiestBackend.connList.Front(); ele != nil; ele = ele.Next() {
			conn := ele.Value
			switch conn.phase {
			case phaseRedirectNotify:
				// A connection cannot be redirected again when it has not finished redirecting.
				continue
			case phaseRedirectFail:
				// If it failed recently, it will probably fail this time.
				if conn.lastRedirect.Add(redirectFailMinInterval).After(curTime) {
					continue
				}
			}
			ce = ele
			break
		}
		if ce == nil {
			break
		}
		conn := ce.Value
		router.logger.Debug("begin redirect connection", zap.Uint64("connID", conn.ConnectionID()),
			zap.String("from", busiestBackend.addr), zap.String("to", idlestBackend.addr),
			zap.Int("from_score", busiestBackend.score()), zap.Int("to_score", idlestBackend.score()))
		busiestBackend.connScore--
		router.adjustBackendList(busiestEle, true)
		idlestBackend.connScore++
		router.adjustBackendList(idlestEle, false)
		conn.phase = phaseRedirectNotify
		conn.lastRedirect = curTime
		conn.Redirect(idlestBackend)
		conn.redirectingBackend = idlestBackend
	}
}

func (router *ScoreBasedRouter) removeBackendIfEmpty(be *glist.Element[*backendWrapper]) bool {
	backend := be.Value
	// If connList.Len() == 0, there won't be any outgoing connections.
	// And if also connScore == 0, there won't be any incoming connections.
	if backend.Status() == observer.StatusCannotConnect && backend.connList.Len() == 0 && backend.connScore <= 0 {
		router.backends.Remove(be)
		return true
	}
	return false
}

func (router *ScoreBasedRouter) ConnCount() int {
	router.Lock()
	defer router.Unlock()
	j := 0
	for be := router.backends.Front(); be != nil; be = be.Next() {
		backend := be.Value
		j += backend.connList.Len()
	}
	return j
}

func (router *ScoreBasedRouter) ServerVersion() string {
	router.Lock()
	version := router.serverVersion
	router.Unlock()
	return version
}

// Close implements Router.Close interface.
func (router *ScoreBasedRouter) Close() {
	if router.cancelFunc != nil {
		router.cancelFunc()
		router.cancelFunc = nil
	}
	router.wg.Wait()
	// Router only refers to RedirectableConn, it doesn't manage RedirectableConn.
}
