// Copyright © 2019 Circonus, Inc. <support@circonus.com>
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
//

// Package ksm is the kube-state-metrics collector
package ksm

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/circonus-labs/circonus-kubernetes-agent/internal/circonus"
	"github.com/circonus-labs/circonus-kubernetes-agent/internal/config"
	"github.com/circonus-labs/circonus-kubernetes-agent/internal/k8s"
	"github.com/circonus-labs/circonus-kubernetes-agent/internal/promtext"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
)

type KSM struct {
	config   *config.Cluster
	mbufSize int
	tbufSize int
	check    *circonus.Check
	log      zerolog.Logger
	running  bool
	sync.Mutex
	ts *time.Time
}

// NOTES:
// curl -v localhost:8080/api/v1/services?fieldSelector=metadata.name%3Dkube-state-metrics
// the spec.ports.name
// combine selfLink with ':http-metrics/proxy/metrics' for metrics
// combine selfLink with ':telemetry/proxy/metrics' for ksm telemetry

func New(cfg *config.Cluster, parentLogger zerolog.Logger, check *circonus.Check) (*KSM, error) {
	if cfg == nil {
		return nil, errors.New("invalid cluster config (nil)")
	}
	if check == nil {
		return nil, errors.New("invalid check (nil)")
	}

	ksm := &KSM{
		config:   cfg,
		mbufSize: 32768,
		tbufSize: 32768,
		check:    check,
		log:      parentLogger.With().Str("collector", "kube-state-metrics").Logger(),
	}

	return ksm, nil
}

func (ksm *KSM) ID() string {
	return "kube-state-metrics"
}

// Collect metrics from kube-state-metrics
func (ksm *KSM) Collect(ctx context.Context, tlsConfig *tls.Config, ts *time.Time) {
	ksm.Lock()
	if ksm.running {
		ksm.log.Warn().Msg("already running")
		ksm.Unlock()
		return
	}
	ksm.running = true
	ksm.ts = ts
	ksm.Unlock()

	defer func() {
		if r := recover(); r != nil {
			ksm.log.Error().Interface("panic", r).Msg("recover")
			ksm.Lock()
			ksm.running = false
			ksm.Unlock()
		}
	}()

	collectStart := time.Now()
	svc, err := ksm.getServiceDefinition(tlsConfig)
	if err != nil {
		ksm.log.Error().Err(err).Msg("service definition")
		ksm.Lock()
		ksm.running = false
		ksm.Unlock()
		return
	}
	if svc == nil {
		ksm.log.Error().Msg("invalid service definition (nil)")
		ksm.Lock()
		ksm.running = false
		ksm.Unlock()
		return
	}

	metricPath := "/proxy/metrics"
	metricPortName := ""
	telemetryPortName := ""
	for _, p := range svc.Spec.Ports {
		switch p.Name {
		case "http-metrics":
			metricPortName = p.Name
		case "telemetry":
			telemetryPortName = p.Name
		}
	}

	var wg sync.WaitGroup

	if metricPortName != "" {
		wg.Add(1)
		go func() {
			metricURL := ksm.config.URL + svc.Metadata.SelfLink + ":" + metricPortName + metricPath
			if err := ksm.metrics(ctx, tlsConfig, metricURL); err != nil {
				ksm.log.Error().Err(err).Str("url", metricURL).Msg("http-metrics")
			}
			wg.Done()
		}()
	}

	if telemetryPortName != "" {
		wg.Add(1)
		go func() {
			telemetryURL := ksm.config.URL + svc.Metadata.SelfLink + ":" + telemetryPortName + metricPath
			if err := ksm.telemetry(ctx, tlsConfig, telemetryURL); err != nil {
				ksm.log.Error().Err(err).Str("url", telemetryURL).Msg("telemetry")
			}
			wg.Done()
		}()
	}

	wg.Wait()

	ksm.log.Info().Str("duration", time.Since(collectStart).String()).Msg("kube-state-metrics collect end")
	ksm.Lock()
	ksm.running = false
	ksm.Unlock()
}

func (ksm *KSM) getServiceDefinition(tlsConfig *tls.Config) (*k8s.Service, error) {
	u, err := url.Parse(ksm.config.URL + "/api/v1/services")
	if err != nil {
		return nil, err
	}

	q := u.Query()
	q.Set("fieldSelector", "metadata.name=kube-state-metrics")
	u.RawQuery = q.Encode()

	client, err := k8s.NewAPIClient(tlsConfig)
	if err != nil {
		return nil, errors.Wrap(err, "service definition cli")
	}
	defer client.CloseIdleConnections()

	reqURL := u.String()
	req, err := k8s.NewAPIRequest(ksm.config.BearerToken, reqURL)
	if err != nil {
		return nil, errors.Wrap(err, "service definition req")
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		data, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			ksm.log.Error().Err(err).Str("url", reqURL).Msg("reading response")
			return nil, err
		}
		ksm.log.Warn().Str("status", resp.Status).RawJSON("response", data).Msg("error from API server")
		return nil, errors.New("error response from api server")
	}

	var s k8s.ServiceList
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return nil, err
	}

	if len(s.Items) == 0 {
		return nil, errors.New("no 'kube-state-metrics' service found")
	}

	if len(s.Items) > 1 {
		return nil, fmt.Errorf("multiple (%d) 'kube-state-metrics' services found", len(s.Items))
	}

	return s.Items[0], nil
}

func (ksm *KSM) metrics(ctx context.Context, tlsConfig *tls.Config, metricURL string) error {
	client, err := k8s.NewAPIClient(tlsConfig)
	if err != nil {
		return errors.Wrap(err, "/metrics cli")
	}
	defer client.CloseIdleConnections()

	req, err := k8s.NewAPIRequest(ksm.config.BearerToken, metricURL)
	if err != nil {
		return errors.Wrap(err, "/metrics req")
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		data, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			ksm.log.Error().Err(err).Str("url", metricURL).Msg("reading response")
			return err
		}
		ksm.log.Warn().Str("status", resp.Status).RawJSON("response", data).Msg("error from API server")
		return errors.New("error response from api server")
	}

	streamTags := []string{"source:kube-state-metrics", "source_type:metrics"}
	measurementTags := []string{}

	if ksm.check.StreamMetrics() {
		var buf bytes.Buffer
		buf.Grow(ksm.mbufSize)

		if err := promtext.StreamMetrics(ctx, &buf, ksm.log, resp.Body, ksm.check, streamTags, measurementTags, ksm.ts); err != nil {
			return err
		}

		if buf.Len() > 0 {
			ksm.mbufSize = buf.Len() // save for next allocation to minimize dynamic growth
			if err := ksm.check.SubmitStream(&buf, ksm.log); err != nil {
				ksm.log.Warn().Err(err).Msg("submitting metrics")
			}
		} else {
			ksm.log.Warn().Msg("no telemetry to submit")
		}

		return nil
	}

	metrics := make(map[string]circonus.MetricSample)
	if err := promtext.QueueMetrics(ctx, metrics, ksm.log, resp.Body, ksm.check, streamTags, measurementTags, nil); err != nil {
		return err
	}
	if len(metrics) > 0 {
		if err := ksm.check.SubmitQueue(metrics, ksm.log); err != nil {
			ksm.log.Warn().Err(err).Msg("submitting metrics")
		}
	} else {
		ksm.log.Warn().Msg("no telemetry to submit")
	}

	return nil
}

func (ksm *KSM) telemetry(ctx context.Context, tlsConfig *tls.Config, telemetryURL string) error {
	client, err := k8s.NewAPIClient(tlsConfig)
	if err != nil {
		return errors.Wrap(err, "/telemetry cli")
	}
	defer client.CloseIdleConnections()

	req, err := k8s.NewAPIRequest(ksm.config.BearerToken, telemetryURL)
	if err != nil {
		return errors.Wrap(err, "/telemetry req")
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		data, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			ksm.log.Error().Err(err).Str("url", telemetryURL).Msg("reading response")
			return err
		}
		ksm.log.Warn().Str("status", resp.Status).RawJSON("response", data).Msg("error from API server")
		return errors.New("error response from api server")
	}

	streamTags := []string{"source:kube-state-metrics", "source_type:telemetry"}
	measurementTags := []string{}

	if ksm.check.StreamMetrics() {
		var buf bytes.Buffer
		buf.Grow(ksm.tbufSize)

		if err := promtext.StreamMetrics(ctx, &buf, ksm.log, resp.Body, ksm.check, streamTags, measurementTags, ksm.ts); err != nil {
			return err
		}

		if buf.Len() > 0 {
			ksm.tbufSize = buf.Len() // save for next allocation to minimize dynamic growth
			if err := ksm.check.SubmitStream(&buf, ksm.log); err != nil {
				ksm.log.Warn().Err(err).Msg("submitting metrics")
			}
		} else {
			ksm.log.Warn().Msg("no telemetry to submit")
		}

		return nil
	}

	metrics := make(map[string]circonus.MetricSample)
	if err := promtext.QueueMetrics(ctx, metrics, ksm.log, resp.Body, ksm.check, streamTags, measurementTags, nil); err != nil {
		return err
	}
	if len(metrics) > 0 {
		if err := ksm.check.SubmitQueue(metrics, ksm.log); err != nil {
			ksm.log.Warn().Err(err).Msg("submitting metrics")
		}
	} else {
		ksm.log.Warn().Msg("no telemetry to submit")
	}

	return nil
}
