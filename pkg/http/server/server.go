package server

import (
	"context"
	"io"
	"net/http"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/golang/snappy"
	clientmodel "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"

	"github.com/openshift/telemeter/pkg/metricfamily"
	"github.com/openshift/telemeter/pkg/store"
	"github.com/openshift/telemeter/pkg/store/ratelimited"
	"github.com/openshift/telemeter/pkg/validate"
)

type Server struct {
	store       store.Store
	transformer metricfamily.Transformer
	validator   validate.Validator
	logger      log.Logger
}

func New(logger log.Logger, store store.Store, validator validate.Validator, transformer metricfamily.Transformer) *Server {
	return &Server{
		store:       store,
		transformer: transformer,
		validator:   validator,
		logger:      log.With(logger, "component", "http/server"),
	}
}

func (s *Server) Post(w http.ResponseWriter, req *http.Request) {
	if req.Method != "POST" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	defer req.Body.Close()

	ctx, cancel := context.WithTimeout(req.Context(), 5*time.Second)
	defer cancel()

	partitionKey, transforms, err := s.validator.Validate(ctx, req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var t metricfamily.MultiTransformer
	t.With(transforms)
	t.With(s.transformer)

	// read the response into memory
	format := expfmt.ResponseFormat(req.Header)
	var r io.Reader = req.Body
	if req.Header.Get("Content-Encoding") == "snappy" {
		r = snappy.NewReader(r)
	}
	decoder := expfmt.NewDecoder(r, format)

	errCh := make(chan error)
	go func() { errCh <- s.decodeAndStoreMetrics(ctx, partitionKey, decoder, t) }()

	select {
	case <-ctx.Done():
		http.Error(w, "Timeout while storing metrics", http.StatusInternalServerError)
		level.Error(s.logger).Log("msg", "timeout processing incoming request")
		return
	case err := <-errCh:
		switch err {
		case nil:
			break
		case ratelimited.ErrWriteLimitReached(partitionKey):
			http.Error(w, err.Error(), http.StatusTooManyRequests)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
}

func (s *Server) decodeAndStoreMetrics(ctx context.Context, partitionKey string, decoder expfmt.Decoder, transformer metricfamily.Transformer) error {
	families := make([]*clientmodel.MetricFamily, 0, 100)
	for {
		family := &clientmodel.MetricFamily{}
		families = append(families, family)
		if err := decoder.Decode(family); err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
	}

	if err := metricfamily.Filter(families, transformer); err != nil {
		return err
	}
	families = metricfamily.Pack(families)

	return s.store.WriteMetrics(ctx, &store.PartitionedMetrics{
		PartitionKey: partitionKey,
		Families:     families,
	})
}
