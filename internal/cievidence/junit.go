package cievidence

import (
	"encoding/xml"
	"fmt"
	"io"
	"os"
)

type junitCase struct {
	Failure []struct{} `xml:"failure"`
	Error   []struct{} `xml:"error"`
	Skipped []struct{} `xml:"skipped"`
}

type junitSuite struct {
	Suites []junitSuite `xml:"testsuite"`
	Cases  []junitCase  `xml:"testcase"`
}

// ReadJUnit counts a JUnit XML report's test cases (a <testsuites> or a
// single <testsuite> root, nested suites included). It counts the cases
// themselves rather than trusting the summary attributes. Nothing else of
// the report, such as test names or output, is kept.
func ReadJUnit(path string) (*JUnit, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var root junitSuite
	if err := xml.NewDecoder(io.LimitReader(f, 64<<20)).Decode(&root); err != nil {
		return nil, fmt.Errorf("cievidence: %s is not JUnit XML: %w", path, err)
	}
	j := &JUnit{}
	var walk func(s junitSuite)
	walk = func(s junitSuite) {
		for _, c := range s.Cases {
			j.Tests++
			switch {
			case len(c.Error) > 0:
				j.Errors++
			case len(c.Failure) > 0:
				j.Failures++
			case len(c.Skipped) > 0:
				j.Skipped++
			}
		}
		for _, sub := range s.Suites {
			walk(sub)
		}
	}
	walk(root)
	return j, nil
}
