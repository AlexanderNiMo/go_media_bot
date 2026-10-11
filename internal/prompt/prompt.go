// Package prompt is the interactive chooser the CLI uses when a query matches
// more than one title.
//
// It is written here rather than pulled in because the whole requirement is
// "print numbered options, read a number". A terminal UI library would bring
// key handling, alternate screen buffers and a rendering model that nothing in
// this project needs, in exchange for a dependency that has to keep working on
// three operating systems for the two years a request can live.
package prompt

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// ErrAborted reports that the user declined to choose.
//
// It is a distinct error rather than a nil result because the caller has to
// tell "the user picked nothing" from "the user picked the first option", and
// an index of zero would mean the latter.
var ErrAborted = errors.New("nothing was chosen")

// maxAttempts bounds how many times a malformed answer is re-asked.
//
// The bound exists for the case where input is not a terminal at all: a pipe
// that has closed gives an immediate EOF, but a pipe that is simply empty
// gives an endless stream of empty lines, and an unbounded loop would turn a
// forgotten flag into a hung process.
const maxAttempts = 5

// Prompter reads choices from a stream and writes the menu to another.
//
// Both are injected rather than being os.Stdin and os.Stdout so that the flow
// can be tested against a scripted answer, which is the only way to test the
// interesting paths — an invalid entry, a cancelled menu — without a terminal.
type Prompter struct {
	in  *bufio.Reader
	out io.Writer
}

// New builds a prompter.
func New(in io.Reader, out io.Writer) *Prompter {
	return &Prompter{in: bufio.NewReader(in), out: out}
}

// Out is where the menu is written.
//
// Exposed so that a caller can print the context a choice is being made in —
// the title that was matched, say — without opening a second writer, which
// would interleave badly with the menu it is there to explain.
func (p *Prompter) Out() io.Writer { return p.out }

// Choose shows numbered options and returns the zero-based index of the one
// picked. It returns ErrAborted when the user cancels.
//
// The options are rendered by the caller, already formatted: this package has
// no opinion about what a title looks like, and formatting a media card here
// would put presentation in two places.
func (p *Prompter) Choose(question string, options []string) (int, error) {
	if len(options) == 0 {
		return 0, errors.New("no options to choose from")
	}

	fmt.Fprintln(p.out, question)
	for i, opt := range options {
		fmt.Fprintf(p.out, "  %d) %s\n", i+1, opt)
	}

	for attempt := 0; attempt < maxAttempts; attempt++ {
		fmt.Fprintf(p.out, "choose 1-%d, or q to cancel: ", len(options))

		line, err := p.in.ReadString('\n')
		if err != nil && line == "" {
			// End of input. The caller is a pipe, a closed terminal or a
			// cancelled script, and none of those will answer a further
			// question either.
			fmt.Fprintln(p.out)
			return 0, fmt.Errorf("%w: end of input", ErrAborted)
		}

		switch answer := strings.TrimSpace(line); {
		case answer == "q" || answer == "quit":
			return 0, ErrAborted
		case answer == "":
			fmt.Fprintln(p.out, "  enter a number")
		default:
			n, err := strconv.Atoi(answer)
			if err != nil {
				fmt.Fprintf(p.out, "  %q is not a number\n", answer)
				continue
			}
			if n < 1 || n > len(options) {
				fmt.Fprintf(p.out, "  %d is not between 1 and %d\n", n, len(options))
				continue
			}
			return n - 1, nil
		}
	}

	return 0, fmt.Errorf("%w: no valid answer in %d attempts; pass an explicit id instead",
		ErrAborted, maxAttempts)
}

// Confirm asks a yes/no question, returning def when the answer is empty.
//
// An unanswered question takes the default rather than being re-asked: this is
// used for "start the request?", where the empty answer is the one a user
// gives by hitting return, and treating it as an error would make the common
// case need a keystroke more than it should.
func (p *Prompter) Confirm(question string, def bool) (bool, error) {
	hint := "y/N"
	if def {
		hint = "Y/n"
	}

	for attempt := 0; attempt < maxAttempts; attempt++ {
		fmt.Fprintf(p.out, "%s [%s]: ", question, hint)

		line, err := p.in.ReadString('\n')
		if err != nil && line == "" {
			fmt.Fprintln(p.out)
			return false, fmt.Errorf("%w: end of input", ErrAborted)
		}

		switch strings.ToLower(strings.TrimSpace(line)) {
		case "":
			return def, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		default:
			fmt.Fprintln(p.out, "  answer y or n")
		}
	}

	return false, fmt.Errorf("%w: no valid answer in %d attempts", ErrAborted, maxAttempts)
}
