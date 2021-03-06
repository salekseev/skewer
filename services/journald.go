package services

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/inconshreveable/log15"
	"github.com/oklog/ulid"
	"github.com/stephane-martin/skewer/conf"
	"github.com/stephane-martin/skewer/journald"
	"github.com/stephane-martin/skewer/metrics"
	"github.com/stephane-martin/skewer/model"
)

func EntryToSyslog(entry map[string]string) *model.SyslogMessage {
	m := model.SyslogMessage{}
	properties := map[string]string{}
	for k, v := range entry {
		k = strings.ToLower(k)
		switch k {
		case "syslog_identifier":
		case "_comm":
			m.Appname = v
		case "message":
			m.Message = v
		case "syslog_pid":
		case "_pid":
			m.Procid = v
		case "priority":
			p, err := strconv.Atoi(v)
			if err == nil {
				m.Severity = model.Severity(p)
			}
		case "syslog_facility":
			f, err := strconv.Atoi(v)
			if err == nil {
				m.Facility = model.Facility(f)
			}
		case "_hostname":
			m.Hostname = v
		case "_source_realtime_timestamp": // microseconds
			t, err := strconv.ParseInt(v, 10, 64)
			if err == nil {
				m.TimeReported = time.Unix(0, t*1000)
			}
		default:
			if strings.HasPrefix(k, "_") {
				properties[k] = v
			}

		}
	}
	if len(m.Appname) == 0 {
		m.Appname = entry["SYSLOG_IDENTIFIER"]
	}
	if len(m.Procid) == 0 {
		m.Procid = entry["SYSLOG_PID"]
	}
	m.TimeGenerated = time.Now()
	if m.TimeReported.IsZero() {
		m.TimeReported = m.TimeGenerated
	}
	m.Priority = model.Priority(int(m.Facility)*8 + int(m.Severity))
	m.Properties = map[string]interface{}{}
	m.Properties["journald"] = properties
	return &m
}

type JournalService struct {
	stasher   model.Stasher
	reader    journald.JournaldReader
	metrics   *metrics.Metrics
	logger    log15.Logger
	stopchan  chan struct{}
	Conf      *conf.JournaldConfig
	wgroup    *sync.WaitGroup
	generator chan ulid.ULID
}

func NewJournalService(
	ctx context.Context,
	stasher model.Stasher,
	generator chan ulid.ULID,
	metric *metrics.Metrics,
	logger log15.Logger) (*JournalService, error) {

	var err error
	s := JournalService{stasher: stasher, metrics: metric, generator: generator}
	s.logger = logger.New("class", "journald")
	s.reader, err = journald.NewReader(ctx, s.logger)
	if err != nil {
		return nil, err
	}
	s.wgroup = &sync.WaitGroup{}
	s.reader.Start()
	return &s, nil
}

func (s *JournalService) Start(test bool) ([]*model.ListenerInfo, error) {
	s.wgroup.Add(1)
	s.stopchan = make(chan struct{})
	go func() {
		defer s.wgroup.Done()

		for {
			select {
			case entry, more := <-s.reader.Entries():
				if more {
					message := EntryToSyslog(entry)
					uid := <-s.generator
					parsedMessage := model.ParsedMessage{
						Client:         "journald",
						LocalPort:      0,
						UnixSocketPath: "",
						Fields:         message,
					}
					fullParsedMessage := model.TcpUdpParsedMessage{
						ConfId: s.Conf.ConfID,
						Uid:    uid.String(),
						Parsed: &parsedMessage,
					}
					if s.stasher != nil {
						s.stasher.Stash(&fullParsedMessage)
					}
					if s.metrics != nil {
						s.metrics.IncomingMsgsCounter.WithLabelValues("journald", "journald", "", "").Inc()
					}
				} else {
					return
				}
			case <-s.stopchan:
				return
			}
		}
	}()
	s.logger.Debug("Journald service is started")
	return []*model.ListenerInfo{}, nil
}

func (s *JournalService) Stop() {
	close(s.stopchan)
	s.wgroup.Wait()
}

func (s *JournalService) WaitClosed() {
	s.wgroup.Wait()
}

func (s *JournalService) SetConf(sc []*conf.SyslogConfig, pc []conf.ParserConfig) {
	s.Conf = &conf.JournaldConfig{
		ConfID:        sc[0].ConfID,
		FilterFunc:    sc[0].FilterFunc,
		PartitionFunc: sc[0].PartitionFunc,
		PartitionTmpl: sc[0].PartitionTmpl,
		TopicFunc:     sc[0].TopicFunc,
		TopicTmpl:     sc[0].TopicTmpl,
	}
}

func (s *JournalService) SetKafkaConf(kc *conf.KafkaConfig) {}

func (s *JournalService) SetAuditConf(ac *conf.AuditConfig) {}
