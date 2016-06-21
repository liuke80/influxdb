package udp // import "github.com/influxdata/influxdb/services/udp"

import (
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/apex/log"
	"github.com/influxdata/influxdb/models"
	"github.com/influxdata/influxdb/services/meta"
	"github.com/influxdata/influxdb/tsdb"
)

const (
	// Arbitrary, testing indicated that this doesn't typically get over 10
	parserChanLen = 1000

	MAX_UDP_PAYLOAD = 64 * 1024
)

// statistics gathered by the UDP package.
const (
	statPointsReceived      = "pointsRx"
	statBytesReceived       = "bytesRx"
	statPointsParseFail     = "pointsParseFail"
	statReadFail            = "readFail"
	statBatchesTransmitted  = "batchesTx"
	statPointsTransmitted   = "pointsTx"
	statBatchesTransmitFail = "batchesTxFail"
)

//
// Service represents here an UDP service
// that will listen for incoming packets
// formatted with the inline protocol
//
type Service struct {
	conn *net.UDPConn
	addr *net.UDPAddr
	wg   sync.WaitGroup
	done chan struct{}

	parserChan chan []byte
	batcher    *tsdb.PointBatcher
	config     Config

	PointsWriter interface {
		WritePoints(database, retentionPolicy string, consistencyLevel models.ConsistencyLevel, points []models.Point) error
	}

	MetaClient interface {
		CreateDatabase(name string) (*meta.DatabaseInfo, error)
	}

	Logger   log.Interface
	stats    *Statistics
	statTags models.Tags
}

// NewService returns a new instance of Service.
func NewService(c Config) *Service {
	d := *c.WithDefaults()
	s := &Service{
		config:     d,
		done:       make(chan struct{}),
		parserChan: make(chan []byte, parserChanLen),
		batcher:    tsdb.NewPointBatcher(d.BatchSize, d.BatchPending, time.Duration(d.BatchTimeout)),
		stats:      &Statistics{},
		statTags:   map[string]string{"bind": d.BindAddress},
	}
	s.WithLogger(log.Log)
	return s
}

// Open starts the service
func (s *Service) Open() (err error) {
	if s.config.BindAddress == "" {
		return errors.New("bind address has to be specified in config")
	}
	if s.config.Database == "" {
		return errors.New("database has to be specified in config")
	}

	if _, err := s.MetaClient.CreateDatabase(s.config.Database); err != nil {
		return errors.New("Failed to ensure target database exists")
	}

	s.addr, err = net.ResolveUDPAddr("udp", s.config.BindAddress)
	if err != nil {
		s.Logger.WithError(err).Errorf("Failed to resolve UDP address %s", s.config.BindAddress)
		return err
	}

	s.conn, err = net.ListenUDP("udp", s.addr)
	if err != nil {
		s.Logger.WithError(err).Errorf("Failed to set up UDP listener at address %s", s.addr)
		return err
	}

	if s.config.ReadBuffer != 0 {
		err = s.conn.SetReadBuffer(s.config.ReadBuffer)
		if err != nil {
			s.Logger.WithError(err).Errorf("Failed to set UDP read buffer to %d",
				s.config.ReadBuffer)
			return err
		}
	}

	s.Logger.Infof("Started listening on UDP: %s", s.config.BindAddress)

	s.wg.Add(3)
	go s.serve()
	go s.parser()
	go s.writer()

	return nil
}

// Statistics maintains statistics for the UDP service.
type Statistics struct {
	PointsReceived      int64
	BytesReceived       int64
	PointsParseFail     int64
	ReadFail            int64
	BatchesTransmitted  int64
	PointsTransmitted   int64
	BatchesTransmitFail int64
}

// Statistics returns statistics for periodic monitoring.
func (s *Service) Statistics(tags map[string]string) []models.Statistic {
	return []models.Statistic{{
		Name: "udp",
		Tags: s.statTags,
		Values: map[string]interface{}{
			statPointsReceived:      atomic.LoadInt64(&s.stats.PointsReceived),
			statBytesReceived:       atomic.LoadInt64(&s.stats.BytesReceived),
			statPointsParseFail:     atomic.LoadInt64(&s.stats.PointsParseFail),
			statReadFail:            atomic.LoadInt64(&s.stats.ReadFail),
			statBatchesTransmitted:  atomic.LoadInt64(&s.stats.BatchesTransmitted),
			statPointsTransmitted:   atomic.LoadInt64(&s.stats.PointsTransmitted),
			statBatchesTransmitFail: atomic.LoadInt64(&s.stats.BatchesTransmitFail),
		},
	}}
}

func (s *Service) writer() {
	defer s.wg.Done()

	for {
		select {
		case batch := <-s.batcher.Out():
			if err := s.PointsWriter.WritePoints(s.config.Database, s.config.RetentionPolicy, models.ConsistencyLevelAny, batch); err == nil {
				atomic.AddInt64(&s.stats.BatchesTransmitted, 1)
				atomic.AddInt64(&s.stats.PointsTransmitted, int64(len(batch)))
			} else {
				s.Logger.WithError(err).Errorf("failed to write point batch to database %q", s.config.Database)
				atomic.AddInt64(&s.stats.BatchesTransmitFail, 1)
			}

		case <-s.done:
			return
		}
	}
}

func (s *Service) serve() {
	defer s.wg.Done()

	buf := make([]byte, MAX_UDP_PAYLOAD)
	s.batcher.Start()
	for {

		select {
		case <-s.done:
			// We closed the connection, time to go.
			return
		default:
			// Keep processing.
			n, _, err := s.conn.ReadFromUDP(buf)
			if err != nil {
				atomic.AddInt64(&s.stats.ReadFail, 1)
				s.Logger.WithError(err).Error("Failed to read UDP message")
				continue
			}
			atomic.AddInt64(&s.stats.BytesReceived, int64(n))

			bufCopy := make([]byte, n)
			copy(bufCopy, buf[:n])
			s.parserChan <- bufCopy
		}
	}
}

func (s *Service) parser() {
	defer s.wg.Done()

	for {
		select {
		case <-s.done:
			return
		case buf := <-s.parserChan:
			points, err := models.ParsePointsWithPrecision(buf, time.Now().UTC(), s.config.Precision)
			if err != nil {
				atomic.AddInt64(&s.stats.PointsParseFail, 1)
				s.Logger.WithError(err).Error("Failed to parse points")
				continue
			}

			for _, point := range points {
				s.batcher.In() <- point
			}
			atomic.AddInt64(&s.stats.PointsReceived, int64(len(points)))
			atomic.AddInt64(&s.stats.PointsReceived, int64(len(points)))
		}
	}
}

// Close closes the underlying listener.
func (s *Service) Close() error {
	if s.conn == nil {
		return errors.New("Service already closed")
	}

	s.conn.Close()
	s.batcher.Flush()
	close(s.done)
	s.wg.Wait()

	// Release all remaining resources.
	s.done = nil
	s.conn = nil

	s.Logger.Info("Service closed")

	return nil
}

// WithLogger sets the logger to augment for log messages. It must not be
// called after the Open method has been called.
func (s *Service) WithLogger(l log.Interface) {
	s.Logger = l.WithField("service", "udp")
}

// Addr returns the listener's address
func (s *Service) Addr() net.Addr {
	return s.addr
}
