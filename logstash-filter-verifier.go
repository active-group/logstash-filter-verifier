// Copyright (c) 2015 Magnus Bäck <magnus@noun.se>

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/alecthomas/kingpin"
	"github.com/magnusbaeck/logstash-filter-verifier/logging"
	"github.com/magnusbaeck/logstash-filter-verifier/logstash"
	"github.com/magnusbaeck/logstash-filter-verifier/testcase"
	oplogging "github.com/op/go-logging"
)

var (
	log = logging.MustGetLogger()

	loglevels = []string{"CRITICAL", "ERROR", "WARNING", "NOTICE", "INFO", "DEBUG"}

	// Flags
	loglevel = kingpin.
			Flag("loglevel", fmt.Sprintf("Set the desired level of logging (one of: %s).", strings.Join(loglevels, ", "))).
			Default("WARNING").
			Enum(loglevels...)
	logstashPath = kingpin.
			Flag("logstash-path", "Set the path to the Logstash executable.").
			Default("/opt/logstash/bin/logstash").
			ExistingFile()

	// Arguments
	testcasePath = kingpin.
			Arg("testcases", "Test case file or a directory containing one or more test case files.").
			Required().
			ExistingFileOrDir()
	configPaths = kingpin.
			Arg("config", "Logstash configuration file or a directory containing one or more configuration files.").
			Required().
			ExistingFilesOrDirs()
)

// runTests runs Logstash with a set of configuration files against a
// slice of test cases and compares the actual events against the
// expected set. Returns an error if at least one test case fails or
// if there's a problem running the tests.
func runTests(logstashPath string, tests []testcase.TestCase, configPaths []string) error {
	ok := true
	for _, t := range tests {
		p, err := logstash.NewProcess(logstashPath, t.Codec, t.Type, configPaths...)
		if err != nil {
			return err
		}
		defer p.Release()
		if err = p.Start(); err != nil {
			return err
		}

		for _, line := range t.InputLines {
			p.Input.Write([]byte(line + "\n"))
		}
		p.Input.Close()

		result, err := p.Wait()
		if err != nil {
			return err
		}
		if err = t.Compare(result.Events, false); err != nil {
			userError("Testcase failed, continuing with the rest: %s", err.Error())
			ok = false
		}
	}
	if !ok {
		return errors.New("One or more testcases failed.")
	}
	return nil
}

// prefixedUserError prints an error message to stderr and prefixes it
// with the name of the program file (e.g. "logstash-filter-verifier:
// something bad happened.").
func prefixedUserError(format string, a ...interface{}) {
	basename := filepath.Base(os.Args[0])
	message := fmt.Sprintf(format, a...)
	if strings.HasSuffix(message, "\n") {
		fmt.Fprintf(os.Stderr, "%s: %s", basename, message)
	} else {
		fmt.Fprintf(os.Stderr, "%s: %s\n", basename, message)
	}
}

// userError prints an error message to stderr.
func userError(format string, a ...interface{}) {
	if strings.HasSuffix(format, "\n") {
		fmt.Fprintf(os.Stderr, format, a...)
	} else {
		fmt.Fprintf(os.Stderr, format+"\n", a...)
	}
}

func main() {
	kingpin.Parse()

	level, err := oplogging.LogLevel(*loglevel)
	if err != nil {
		prefixedUserError("Bad loglevel: %s", loglevel)
		os.Exit(1)
	}
	logging.SetLevel(level)

	tests, err := testcase.DiscoverTests(*testcasePath)
	if err != nil {
		userError(err.Error())
		os.Exit(1)
	}
	if err = runTests(*logstashPath, tests, *configPaths); err != nil {
		userError(err.Error())
		os.Exit(1)
	}
	os.Exit(0)
}
