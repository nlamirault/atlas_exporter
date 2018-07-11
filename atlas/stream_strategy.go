package atlas

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/DNS-OARC/ripeatlas/measurement"
	"github.com/prometheus/common/log"

	"github.com/DNS-OARC/ripeatlas"
)

const ConnectionRetryInterval = 30 * time.Second

type streamingStrategy struct {
	stream  *ripeatlas.Stream
	results map[string]map[int]*measurement.Result
	workers uint
	timeout time.Duration
	mu      sync.RWMutex
}

// NewStreamingStrategy returns an strategy using the RIPE Atlas Streaming API
func NewStreamingStrategy(ctx context.Context, ids []string, workers uint, timeout time.Duration) Strategy {
	s := &streamingStrategy{
		stream:  ripeatlas.NewStream(),
		workers: workers,
		timeout: timeout,
		results: make(map[string]map[int]*measurement.Result),
	}

	s.start(ctx, ids)
	return s
}

func (s *streamingStrategy) start(ctx context.Context, ids []string) {
	for _, id := range ids {
		go s.startListening(ctx, id)
	}
}

func (s *streamingStrategy) startListening(ctx context.Context, id string) {
	for {
		ch, err := s.subscribe(id)
		if err != nil {
			log.Error(err)
		} else {
			log.Infof("Subscribed to results of measurement #%s", id)
			s.listenForResults(ctx, ch)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(ConnectionRetryInterval):
			delete(s.results, id)
			continue
		}
	}
}

func (s *streamingStrategy) subscribe(id string) (<-chan *measurement.Result, error) {
	msm, err := strconv.Atoi(id)
	if err != nil {
		return nil, err
	}

	ch, err := s.stream.MeasurementResults(ripeatlas.Params{
		"msm": msm,
	})
	if err != nil {
		return nil, err
	}

	return ch, nil
}

func (s *streamingStrategy) listenForResults(ctx context.Context, ch <-chan *measurement.Result) {
	for {
		select {
		case m := <-ch:
			if m.ParseError != nil {
				log.Error(m.ParseError)
			}

			if m.ParseError != nil && strings.HasPrefix(m.ParseError.Error(), "c.On(disconnect)") {
				log.Error(m.ParseError)
				return
			}

			s.processMeasurement(m)
		case <-time.After(s.timeout):
			log.Errorf("Timeout reached. Trying to reconnect.")
			return
		case <-ctx.Done():
			return
		}
	}
}

func (s *streamingStrategy) processMeasurement(m *measurement.Result) {
	go s.warmProbeCache(m)
	s.addOrReplace(m)
}

func (s *streamingStrategy) warmProbeCache(m *measurement.Result) {
	_, err := probeForID(m.PrbId())
	if err != nil {
		log.Error(err)
	}
}

func (s *streamingStrategy) addOrReplace(m *measurement.Result) {
	s.mu.Lock()
	defer s.mu.Unlock()

	msm := strconv.Itoa(m.MsmId())

	_, found := s.results[msm]
	if !found {
		s.results[msm] = make(map[int]*measurement.Result)
	}

	s.results[msm][m.PrbId()] = m
}

func (s *streamingStrategy) MeasurementResults(ctx context.Context, ids []string) ([]*AtlasMeasurement, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	measurements := make([]*AtlasMeasurement, 0)
	for _, id := range ids {
		m, found := s.results[id]
		if !found {
			continue
		}

		res := make([]*measurement.Result, 0)
		for _, v := range m {
			res = append(res, v)
		}

		r, err := atlasMeasurementForResults(res, id, s.workers)
		if err != nil {
			return nil, err
		}

		measurements = append(measurements, r)
	}

	return measurements, nil
}