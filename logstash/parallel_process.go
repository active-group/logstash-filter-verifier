// Copyright (c) 2015-2016 Magnus Bäck <magnus@noun.se>

package logstash

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// TestStream contains the input and output streams for one test case
type TestStream struct {
	sender         *net.UnixConn
	senderListener *net.UnixListener
	senderReady    chan struct{}
	senderPath     string
	receiver       *deletedTempFile
	timeout        time.Duration

	inputCodec string
	fields     FieldSet
}

// NewTestStream creates a TestStream, inputCodec is
// the desired codec for the stdin input and inputType the value of
// the "type" field for ingested events.
// The timeout defines, how long to wait in Write for the receiver to
// become available.
func NewTestStream(inputCodec string, fields FieldSet, timeout time.Duration) (*TestStream, error) {
	dir, err := ioutil.TempDir("", "")
	if err != nil {
		return nil, err
	}

	ts := &TestStream{
		senderReady: make(chan struct{}),
		senderPath:  dir,
		inputCodec:  inputCodec,
		fields:      fields,
		timeout:     timeout,
	}

	ts.senderListener, err = net.ListenUnix("unix", &net.UnixAddr{Name: ts.senderPath + "/socket", Net: "unix"})
	if err != nil {
		log.Fatalf("Unable to create unix socket for listening: %s", err)
	}
	ts.senderListener.SetUnlinkOnClose(false)

	go func() {
		defer close(ts.senderReady)

		ts.sender, err = ts.senderListener.AcceptUnix()
		if err != nil {
			log.Errorf("Error while accept unix socket: %s", err)
		}
		ts.senderListener.Close()
	}()

	// Unfortunately Logstash doesn't make it easy to just read
	// events from a stdout-connected pipe and the log from a
	// stderr-connected pipe. Stdout can contain other garbage (at
	// the very least "future logs will be sent to ...") and error
	// messages could very well be sent there too. Mitigate by
	// having Logstash write output logs to a temporary file and
	// its own logs to a different temporary file.
	outputFile, err := newDeletedTempFile("", "")
	if err != nil {
		return nil, err
	}
	ts.receiver = outputFile

	return ts, nil
}

// Write writes to the sender of the TestStream
func (ts *TestStream) Write(p []byte) (n int, err error) {
	timer := time.NewTimer(ts.timeout)
	select {
	case <-ts.senderReady:
	case <-timer.C:
		return 0, fmt.Errorf("Write timeout error")
	}
	return ts.sender.Write(p)
}

// Close closes the sender of the TestStream
func (ts *TestStream) Close() error {
	if ts.sender != nil {
		err := ts.sender.Close()
		ts.sender = nil
		return err
	}
	return nil
}

// Cleanup closes and removes all temporary resources
// for a TestStream
func (ts *TestStream) Cleanup() {
	if ts.senderListener != nil {
		ts.senderListener.Close()
	}
	if ts.sender != nil {
		ts.Close()
	}
	os.RemoveAll(ts.senderPath)
	if ts.receiver != nil {
		ts.receiver.Close()
	}
}

// CleanupTestStreams closes all sockets and streams as well
// removes temporary file ressources for an array of
// TestStreams
func CleanupTestStreams(ts []*TestStream) {
	for i := range ts {
		ts[i].Cleanup()
	}
}

// ParallelProcess represents the invocation and execution of a Logstash child
// process that emits JSON events from multiple inputs through filter to multiple outputs
// configuration files supplied by the caller.
type ParallelProcess struct {
	streams []*TestStream

	child     *exec.Cmd
	configDir *string
	log       io.ReadCloser

	stdio io.Reader
}

// NewParallelProcess prepares for the execution of a new Logstash process but
// doesn't actually start it. logstashPath is the path to the Logstash
// executable (typically /opt/logstash/bin/logstash). The configs parameter is
// one or more configuration files containing Logstash filters.
func NewParallelProcess(logstashPath string, testStream []*TestStream, keptEnvVars []string, configs ...string) (*ParallelProcess, error) {
	if len(configs) == 0 {
		return nil, errors.New("must provide non-empty list of configuration file or directory names")
	}

	logstashInput := make([]string, len(testStream))
	logstashOutput := make([]string, len(testStream))

	for i, sp := range testStream {
		sp.fields["@metadata"] = map[string]interface{}{"__lfv_testcase": strconv.Itoa(i)}
		fieldHash, err := sp.fields.LogstashHash()
		if err != nil {
			CleanupTestStreams(testStream)
			return nil, err
		}
		logstashInput[i] = fmt.Sprintf("unix { mode => \"client\" path => %q codec => %q add_field => %s }", sp.senderPath+"/socket", sp.inputCodec, fieldHash)
		logstashOutput[i] = fmt.Sprintf("if [@metadata][__lfv_testcase] == \"%s\" { file { path => %q codec => \"json_lines\" } }", strconv.Itoa(i), sp.receiver.Name())
	}

	logFile, err := newDeletedTempFile("", "")
	if err != nil {
		CleanupTestStreams(testStream)
		return nil, err
	}

	configDir, err := getConfigFileDir(configs)
	if err != nil {
		CleanupTestStreams(testStream)
		_ = logFile.Close()
		return nil, err
	}

	args := []string{
		"-w", // Make messages arrive in order.
		"1",
		"--debug",
		"-e",
		fmt.Sprintf(
			"input { %s } "+
				"output { %s }",
			strings.Join(logstashInput, " "), strings.Join(logstashOutput, " ")),
		"--config",
		configDir,
		"--log",
		logFile.Name(),
	}

	p, err := newParallelProcessWithArgs(logstashPath, args, getLimitedEnvironment(os.Environ(), keptEnvVars))
	if err != nil {
		CleanupTestStreams(testStream)
		_ = logFile.Close()
	}
	p.configDir = &configDir
	p.log = logFile
	p.streams = testStream
	return p, nil
}

// newParallelProcessWithArgs performs the non-Logstash specific low-level
// actions of preparing to spawn a child process, making it easier to
// test the code in this package.
func newParallelProcessWithArgs(command string, args []string, env []string) (*ParallelProcess, error) {
	c := exec.Command(command, args...)
	c.Env = env

	// Save the process's stdout and stderr since an early startup
	// failure (e.g. JVM issues) will get dumped there and not in
	// the log file.
	var b bytes.Buffer
	c.Stdout = &b
	c.Stderr = &b

	return &ParallelProcess{
		child: c,
		stdio: &b,
	}, nil
}

// Start starts a Logstash child process with the previously supplied
// configuration.
func (p *ParallelProcess) Start() error {
	log.Info("Starting %q with args %q.", p.child.Path, p.child.Args[1:])
	return p.child.Start()
}

// Wait blocks until the started Logstash process terminates and
// returns the result of the execution.
func (p *ParallelProcess) Wait() (*ParallelResult, error) {
	if p.child.Process == nil {
		return nil, errors.New("can't wait on an unborn process")
	}
	log.Debug("Waiting for child with pid %d to terminate.", p.child.Process.Pid)

	waiterr := p.child.Wait()

	// Save the log output regardless of whether the child process
	// succeeded or not.
	logbuf, logerr := ioutil.ReadAll(p.log)
	if logerr != nil {
		// Log this weird error condition but don't let it
		// fail the function. We don't care about the log
		// contents unless Logstash fails, in which we'll
		// report that problem anyway.
		log.Error("Error reading the Logstash logfile: %s", logerr.Error())
	}
	outbuf, _ := ioutil.ReadAll(p.stdio)

	result := ParallelResult{
		Events:  [][]Event{},
		Log:     string(logbuf),
		Output:  string(outbuf),
		Success: waiterr == nil,
	}
	if waiterr != nil {
		re := regexp.MustCompile("An unexpected error occurred.*closed stream.*IOError")
		if re.MatchString(result.Log) {
			log.Warning("Workaround for IOError in unix.rb on stop, process result anyway. (see https://github.com/logstash-plugins/logstash-input-unix/pull/18)")
			result.Success = true
		} else {
			return &result, waiterr
		}
	}

	var err error
	result.Events = make([][]Event, len(p.streams))
	for i, tc := range p.streams {
		result.Events[i], err = readEvents(tc.receiver)
		tc.receiver.Close()
		result.Success = err == nil
	}
	return &result, err
}

// Release frees all allocated resources connected to this process.
func (p *ParallelProcess) Release() {
	CleanupTestStreams(p.streams)
	_ = p.log.Close()
	if p.configDir != nil {
		_ = os.RemoveAll(*p.configDir)
	}
}