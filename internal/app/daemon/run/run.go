package run

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/imkira/go-observer"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"google.golang.org/grpc"

	pb "github.com/magnusbaeck/logstash-filter-verifier/v2/internal/daemon/api/grpc"
	"github.com/magnusbaeck/logstash-filter-verifier/v2/internal/daemon/pipeline"
	"github.com/magnusbaeck/logstash-filter-verifier/v2/internal/logging"
	"github.com/magnusbaeck/logstash-filter-verifier/v2/internal/logstash"
	lfvobserver "github.com/magnusbaeck/logstash-filter-verifier/v2/internal/observer"
	"github.com/magnusbaeck/logstash-filter-verifier/v2/internal/testcase"
)

type Test struct {
	socket       string
	pipeline     string
	pipelineBase string
	testcasePath string
	debug        bool

	log logging.Logger
}

func New(socket string, log logging.Logger, pipeline, pipelineBase, testcasePath string, debug bool) (Test, error) {
	if !path.IsAbs(pipelineBase) {
		cwd, err := os.Getwd()
		if err != nil {
			return Test{}, err
		}
		pipelineBase = path.Join(cwd, pipelineBase)
	}
	return Test{
		socket:       socket,
		pipeline:     pipeline,
		pipelineBase: pipelineBase,
		testcasePath: testcasePath,
		debug:        debug,
		log:          log,
	}, nil
}

func (s Test) Run() error {
	a, err := pipeline.New(s.pipeline, s.pipelineBase)
	if err != nil {
		return err
	}

	// TODO: ensure, that IDs are also unique for the whole set of pipelines
	err = a.Validate()
	if err != nil {
		return err
	}

	b, err := a.Zip()
	if err != nil {
		return err
	}

	s.log.Debugf("socket to daemon %q", s.socket)
	conn, err := grpc.Dial(
		s.socket,
		grpc.WithInsecure(),
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			if d, ok := ctx.Deadline(); ok {
				return net.DialTimeout("unix", addr, time.Until(d))
			}
			return net.Dial("unix", addr)
		}))
	if err != nil {
		return err
	}
	defer conn.Close()
	c := pb.NewControlClient(conn)

	result, err := c.SetupTest(context.Background(), &pb.SetupTestRequest{
		Pipeline: b,
	})
	if err != nil {
		return err
	}
	sessionID := result.SessionID

	tests, err := testcase.DiscoverTests(s.testcasePath)
	if err != nil {
		return err
	}

	observers := make([]lfvobserver.Interface, 0)
	liveObserver := observer.NewProperty(lfvobserver.TestExecutionStart{})
	observers = append(observers, lfvobserver.NewSummaryObserver(liveObserver))
	for _, obs := range observers {
		if err := obs.Start(); err != nil {
			return err
		}
	}

	for _, t := range tests {
		b, err := json.Marshal(t.InputFields)
		if err != nil {
			return err
		}
		result, err := c.ExecuteTest(context.Background(), &pb.ExecuteTestRequest{
			SessionID:  sessionID,
			InputLines: t.InputLines,
			Fields:     b,
		})
		if err != nil {
			return err
		}

		results, err := s.postProcessResults(result.Results)
		if err != nil {
			return err
		}

		var events []logstash.Event
		for _, line := range results {
			var event logstash.Event
			err = json.Unmarshal([]byte(line), &event)
			if err != nil {
				return err
			}
			events = append(events, event)
		}

		_, err = t.Compare(events, []string{"diff", "-u"}, liveObserver)
		if err != nil {
			return err
		}
	}

	_, err = c.TeardownTest(context.Background(), &pb.TeardownTestRequest{
		SessionID: sessionID,
		Stats:     false,
	})
	if err != nil {
		return err
	}

	liveObserver.Update(lfvobserver.TestExecutionEnd{})

	for _, obs := range observers {
		if err := obs.Finalize(); err != nil {
			return err
		}
	}

	return nil
}

func (s Test) postProcessResults(results []string) ([]string, error) {
	var err error

	sort.Slice(results, func(i, j int) bool {
		return gjson.Get(results[i], `__lfv_id`).Int() < gjson.Get(results[j], `__lfv_id`).Int()
	})

	// No cleanup if debug is set
	if s.debug {
		return results, nil
	}

	for i := range results {
		results[i], err = sjson.Delete(results[i], "__lfv_id")
		if err != nil {
			return nil, err
		}

		tags := []string{}
		for _, tag := range gjson.Get(results[i], "tags").Array() {
			if strings.HasPrefix(tag.String(), "__lfv_") {
				continue
			}
			tags = append(tags, tag.String())
		}

		// Remove tag entry, if there are no tags
		if len(tags) == 0 {
			results[i], err = sjson.Delete(results[i], "tags")
		} else {
			results[i], err = sjson.Set(results[i], "tags", tags)
		}
		if err != nil {
			return nil, err
		}
	}

	return results, nil
}
