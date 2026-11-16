package manager

import (
	"context"

	"github.com/Trifocals3537/tessarr/pkg/usenet"
	"github.com/Trifocals3537/tessarr/pkg/usenet/recovery"
)

func (m *Manager) PAR2Targets(ctx context.Context, q, after string) ([]usenet.PAR2Target, string, error) {
	if m == nil || m.usenet == nil {
		return nil, "", usenet.ErrPAR2NotConfigured
	}
	return m.usenet.PAR2Targets(ctx, q, after)
}
func (m *Manager) PAR2Jobs() ([]recovery.Job, error) {
	if m == nil || m.usenet == nil {
		return nil, usenet.ErrPAR2NotConfigured
	}
	if m.usenet.PAR2Service() == nil {
		return nil, recovery.ErrUnavailable
	}
	return m.usenet.PAR2Service().Jobs()
}
func (m *Manager) StartPAR2(ctx context.Context, id, name string, source []byte) (recovery.Job, error) {
	if m == nil || m.usenet == nil {
		return recovery.Job{}, usenet.ErrPAR2NotConfigured
	}
	if m.usenet.PAR2Service() == nil {
		return recovery.Job{}, recovery.ErrUnavailable
	}
	return m.usenet.PAR2Service().Start(ctx, id, name, source)
}
func (m *Manager) CancelPAR2(id string) error {
	if m == nil || m.usenet == nil {
		return usenet.ErrPAR2NotConfigured
	}
	if m.usenet.PAR2Service() == nil {
		return recovery.ErrUnavailable
	}
	return m.usenet.PAR2Service().Cancel(id)
}
