// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"errors"
	"sync"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

type dialogCache[T DialogSession] interface {
	store(ctx context.Context, id string, v T) error
	load(ctx context.Context, id string) (T, error)
	remove(ctx context.Context, id string) error
	rangeDialogs(ctx context.Context, f func(id string, d T) bool) error
}

// Non optimized for now
type dialogCacheMap[T DialogSession] struct{ sync.Map }

func (m *dialogCacheMap[T]) store(ctx context.Context, id string, v T) error {
	m.Store(id, v)
	return nil
}

func (m *dialogCacheMap[T]) remove(ctx context.Context, id string) error {
	m.Delete(id)
	return nil
}

func (m *dialogCacheMap[T]) load(ctx context.Context, id string) (dialog T, err error) {
	d, ok := m.Load(id)
	if !ok {
		return dialog, sipgo.ErrDialogDoesNotExists
	}
	// interface to interface conversion is slow
	return d.(T), nil
}

func (m *dialogCacheMap[T]) rangeDialogs(ctx context.Context, f func(id string, d T) bool) error {
	m.Range(func(key, value any) bool {
		return f(key.(string), value.(T))
	})
	return nil
}

type dialogData struct {
	InviteRequest sip.Request
	State         sip.DialogState
}

// dialogCachePool is the internal index of live dialogs. Requests are
// resolved through matchDialog; the pool itself is not part of the public API.
type dialogCachePool struct {
	client dialogCache[*DialogClientSession]
	server dialogCache[*DialogServerSession]
}

func (p *dialogCachePool) matchDialogClient(req *sip.Request) (*DialogClientSession, error) {
	id, err := sip.DialogIDFromRequestUAC(req)
	if err != nil {
		return nil, errors.Join(err, sipgo.ErrDialogOutsideDialog)
	}

	val, err := p.client.load(context.Background(), id)
	if err != nil {
		return nil, err
	}

	return val, nil
}

func (p *dialogCachePool) matchDialogServer(req *sip.Request) (*DialogServerSession, error) {
	id, err := sip.DialogIDFromRequestUAS(req)
	if err != nil {
		return nil, errors.Join(err, sipgo.ErrDialogOutsideDialog)
	}

	val, err := p.server.load(context.Background(), id)
	if err != nil {
		return nil, err
	}

	return val, nil
}

func (p *dialogCachePool) matchDialog(req *sip.Request) (*DialogServerSession, *DialogClientSession, error) {
	d, err := p.matchDialogServer(req)
	if err != nil {
		if !errors.Is(err, sipgo.ErrDialogDoesNotExists) {
			return nil, nil, err
		}

		cd, err := p.matchDialogClient(req)
		return nil, cd, err
	}
	return d, nil, nil
}
