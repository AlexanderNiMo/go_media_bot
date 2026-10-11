package prompt

import (
	"bytes"
	"strings"
	"testing"
)

func newTest(answers string) (*Prompter, *bytes.Buffer) {
	out := &bytes.Buffer{}
	return New(strings.NewReader(answers), out), out
}

func TestChooseReturnsThePickedIndex(t *testing.T) {
	p, out := newTest("2\n")

	got, err := p.Choose("which one?", []string{"first", "second", "third"})
	if err != nil {
		t.Fatalf("Choose: %v", err)
	}
	if got != 1 {
		t.Errorf("want the second option (index 1), got %d", got)
	}

	// The menu is what the user is answering, so it has to have been printed.
	for _, want := range []string{"which one?", "1) first", "3) third"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the menu is missing %q; it printed:\n%s", want, out)
		}
	}
}

// TestChooseRetriesOnNonsense — a typo must not be fatal, and must not be
// silently treated as one of the options either.
func TestChooseRetriesOnNonsense(t *testing.T) {
	p, out := newTest("banana\n9\n0\n3\n")

	got, err := p.Choose("pick", []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("Choose: %v", err)
	}
	if got != 2 {
		t.Errorf("want index 2 after the invalid entries, got %d", got)
	}

	printed := out.String()
	for _, want := range []string{`"banana" is not a number`, "9 is not between 1 and 3", "0 is not between 1 and 3"} {
		if !strings.Contains(printed, want) {
			t.Errorf("expected the menu to say %q; it printed:\n%s", want, printed)
		}
	}
}

// TestChooseAborts — an explicit cancel is ErrAborted, not an error with a
// message, so the caller can exit quietly instead of printing a failure.
func TestChooseAborts(t *testing.T) {
	for _, answer := range []string{"q\n", "quit\n"} {
		p, _ := newTest(answer)

		_, err := p.Choose("pick", []string{"a", "b"})
		if err == nil {
			t.Fatalf("%q: expected an abort", answer)
		}
		if !isAborted(err) {
			t.Errorf("%q: want ErrAborted, got %v", answer, err)
		}
	}
}

// TestChooseOnExhaustedInput is the failure that matters operationally: the
// command was run from a script or with stdin closed, and it must return
// rather than spin.
func TestChooseOnExhaustedInput(t *testing.T) {
	p, _ := newTest("")

	_, err := p.Choose("pick", []string{"a", "b"})
	if !isAborted(err) {
		t.Fatalf("want an abort on end of input, got %v", err)
	}
}

// TestChooseOnEndlessEmptyInput — a pipe that never closes and never answers.
// Without the attempt bound this is an infinite loop, which is how a missing
// flag becomes a hung process.
func TestChooseOnEndlessEmptyInput(t *testing.T) {
	p, _ := newTest(strings.Repeat("\n", 1000))

	_, err := p.Choose("pick", []string{"a", "b"})
	if !isAborted(err) {
		t.Fatalf("want an abort, got %v", err)
	}
	if !strings.Contains(err.Error(), "attempts") {
		t.Errorf("the error should say the attempts ran out, got %v", err)
	}
}

func TestChooseWithNoOptions(t *testing.T) {
	p, _ := newTest("1\n")

	if _, err := p.Choose("pick", nil); err == nil {
		t.Fatal("expected an error when there is nothing to choose from")
	}
}

func TestConfirm(t *testing.T) {
	cases := []struct {
		name    string
		answers string
		def     bool
		want    bool
	}{
		{name: "yes", answers: "y\n", def: false, want: true},
		{name: "yes spelled out", answers: "yes\n", def: false, want: true},
		{name: "no", answers: "n\n", def: true, want: false},
		{name: "empty takes the default true", answers: "\n", def: true, want: true},
		{name: "empty takes the default false", answers: "\n", def: false, want: false},
		{name: "retries then answers", answers: "maybe\ny\n", def: false, want: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := newTest(tc.answers)

			got, err := p.Confirm("sure?", tc.def)
			if err != nil {
				t.Fatalf("Confirm: %v", err)
			}
			if got != tc.want {
				t.Errorf("want %v, got %v", tc.want, got)
			}
		})
	}
}

func TestConfirmOnExhaustedInput(t *testing.T) {
	p, _ := newTest("")

	if _, err := p.Confirm("sure?", true); !isAborted(err) {
		t.Fatalf("want an abort on end of input, got %v", err)
	}
}

func isAborted(err error) bool {
	return err != nil && strings.Contains(err.Error(), ErrAborted.Error())
}
