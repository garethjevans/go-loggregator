package loggregator

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"strings"
	"time"

	"code.cloudfoundry.org/go-loggregator/rpc/loggregator_v2"
	"github.com/golang/protobuf/jsonpb"
	"golang.org/x/net/context"
)

type RLPGatewayClient struct {
	addr       string
	log        Logger
	doer       Doer
	maxRetries int
}

type GatewayLogger interface {
	Printf(format string, v ...interface{})
	Panicf(format string, v ...interface{})
}

func NewRLPGatewayClient(addr string, opts ...RLPGatewayClientOption) *RLPGatewayClient {
	c := &RLPGatewayClient{
		addr:       addr,
		log:        log.New(ioutil.Discard, "", 0),
		doer:       http.DefaultClient,
		maxRetries: 10,
	}

	for _, o := range opts {
		o(c)
	}

	return c
}

// RLPGatewayClientOption is the type of a configurable client option.
type RLPGatewayClientOption func(*RLPGatewayClient)

// WithRLPGatewayClientLogger returns a RLPGatewayClientOption to configure
// the logger of the RLPGatewayClient. It defaults to a silent logger.
func WithRLPGatewayClientLogger(log GatewayLogger) RLPGatewayClientOption {
	return func(c *RLPGatewayClient) {
		c.log = log
	}
}

// WithRLPGatewayClientLogger returns a RLPGatewayClientOption to configure
// the HTTP client. It defaults to the http.DefaultClient.
func WithRLPGatewayHTTPClient(d Doer) RLPGatewayClientOption {
	return func(c *RLPGatewayClient) {
		c.doer = d
	}
}

// WithRLPGatewayMaxRetries returns a RLPGatewayClientOption to configure
// how many times the client will attempt to connect to the RLP gateway
// before giving up.
func WithRLPGatewayMaxRetries(r int) RLPGatewayClientOption {
	return func(c *RLPGatewayClient) {
		c.maxRetries = r
	}
}

// Doer is used to make HTTP requests to the RLP Gateway.
type Doer interface {
	// Do is a implementation of the http.Client's Do method.
	Do(*http.Request) (*http.Response, error)
}

// Stream returns a new EnvelopeStream for the given context and request. The
// lifecycle of the EnvelopeStream is managed by the given context. If the
// underlying SSE stream dies, it attempts to reconnect until the context
// is done. Any errors are logged via the client's logger.
func (c *RLPGatewayClient) Stream(ctx context.Context, req *loggregator_v2.EgressBatchRequest) EnvelopeStream {
	var numRetries int
	es := make(chan *loggregator_v2.Envelope, 100)
	go func() {
		defer close(es)
		for ctx.Err() == nil && numRetries <= c.maxRetries {
			connectionSucceeded := c.connect(ctx, es, req)
			if connectionSucceeded {
				numRetries = 0
				continue
			}
			numRetries++
		}
	}()

	return func() []*loggregator_v2.Envelope {
		var batch []*loggregator_v2.Envelope
		for {
			select {
			case <-ctx.Done():
				return nil
			case e, ok := <-es:
				if !ok {
					return nil
				}
				batch = append(batch, e)
			default:
				if len(batch) > 0 {
					return batch
				}

				time.Sleep(50 * time.Millisecond)
			}
		}
	}
}

func (c *RLPGatewayClient) connect(
	ctx context.Context,
	es chan<- *loggregator_v2.Envelope,
	logReq *loggregator_v2.EgressBatchRequest,
) bool {
	readAddr := fmt.Sprintf("%s/v2/read%s", c.addr, c.buildQuery(logReq))

	req, err := http.NewRequest(http.MethodGet, readAddr, nil)
	if err != nil {
		c.log.Panicf("failed to build request %s", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")

	resp, err := c.doer.Do(req.WithContext(ctx))
	if err != nil {
		c.log.Printf("error making request: %s", err)
		return false
	}

	defer func() {
		io.Copy(ioutil.Discard, resp.Body)
		resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			c.log.Printf("failed to read body: %s", err)
			return false
		}
		c.log.Printf("unexpected status code %d: %s", resp.StatusCode, body)
		return false
	}

	buf := bytes.NewBuffer(nil)
	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			c.log.Printf("failed while reading stream: %s", err)
			return true
		}

		switch {
		case bytes.HasPrefix(line, []byte("heartbeat: ")):
			// TODO: Remove this old case
			continue
		case bytes.HasPrefix(line, []byte("event: closing")):
			return true
		case bytes.HasPrefix(line, []byte("event: heartbeat")):
			// Throw away the data of the heartbeat event and the next
			// newline.
			_, _ = reader.ReadBytes('\n')
			_, _ = reader.ReadBytes('\n')
			continue
		case bytes.HasPrefix(line, []byte("data: ")):
			buf.Write(line[len("data: "):])
		case bytes.Equal(line, []byte("\n")):
			if buf.Len() == 0 {
				continue
			}

			var eb loggregator_v2.EnvelopeBatch
			if err := jsonpb.Unmarshal(buf, &eb); err != nil {
				c.log.Printf("failed to unmarshal envelope: %s", err)
				continue
			}

			for _, e := range eb.Batch {
				select {
				case <-ctx.Done():
					return true
				case es <- e:
				}
			}
		}

	}
}

func (c *RLPGatewayClient) buildQuery(req *loggregator_v2.EgressBatchRequest) string {
	var query []string
	if req.GetShardId() != "" {
		query = append(query, "shard_id="+req.GetShardId())
	}

	if req.GetDeterministicName() != "" {
		query = append(query, "deterministic_name="+req.GetDeterministicName())
	}

	for _, selector := range req.GetSelectors() {
		if selector.GetSourceId() != "" {
			query = append(query, "source_id="+selector.GetSourceId())
		}

		switch selector.Message.(type) {
		case *loggregator_v2.Selector_Log:
			query = append(query, "log")
		case *loggregator_v2.Selector_Counter:
			if selector.GetCounter().GetName() != "" {
				query = append(query, "counter.name="+selector.GetCounter().GetName())
				continue
			}
			query = append(query, "counter")
		case *loggregator_v2.Selector_Gauge:
			if len(selector.GetGauge().GetNames()) > 1 {
				// TODO: This is a mistake in the gateway.
				panic("This is not yet supported")
			}

			if len(selector.GetGauge().GetNames()) != 0 {
				query = append(query, "gauge.name="+selector.GetGauge().GetNames()[0])
				continue
			}
			query = append(query, "gauge")
		case *loggregator_v2.Selector_Timer:
			query = append(query, "timer")
		case *loggregator_v2.Selector_Event:
			query = append(query, "event")
		}
	}

	namedCounter := containsPrefix(query, "counter.name")
	namedGauge := containsPrefix(query, "gauge.name")

	if namedCounter {
		query = filter(query, "counter")
	}

	if namedGauge {
		query = filter(query, "gauge")
	}

	query = removeDuplicateSourceIDs(query)
	if len(query) == 0 {
		return ""
	}

	return "?" + strings.Join(query, "&")
}

func removeDuplicateSourceIDs(query []string) []string {
	sids := map[string]bool{}
	duplicates := 0
	for i, j := 0, 0; i < len(query); i++ {
		if strings.HasPrefix(query[i], "source_id=") && sids[query[i]] {
			// Duplicate source ID
			duplicates++
			continue
		}
		sids[query[i]] = true
		query[j] = query[i]
		j++
	}

	return query[:len(query)-duplicates]
}

func containsPrefix(arr []string, prefix string) bool {
	for _, i := range arr {
		if strings.HasPrefix(i, prefix) {
			return true
		}
	}
	return false
}

func filter(arr []string, target string) []string {
	var filtered []string
	for _, i := range arr {
		if i != target {
			filtered = append(filtered, i)
		}
	}
	return filtered
}
