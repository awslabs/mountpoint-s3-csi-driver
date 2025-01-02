package mountpoint

import (
	"strings"

	"k8s.io/apimachinery/pkg/util/sets"
)

const (
	ArgForeground      = "--foreground"
	ArgReadOnly        = "--read-only"
	ArgAllowOther      = "--allow-other"
	ArgAllowRoot       = "--allow-root"
	ArgRegion          = "--region"
	ArgCache           = "--cache"
	ArgUserAgentPrefix = "--user-agent-prefix"
	ArgAWSMaxAttempts  = "--aws-max-attempts"
)

// An Args represents arguments to be passed to Mountpoint during mount.
type Args struct {
	args sets.Set[string]
}

// ParseArgs parses given list of unnormalized and returns a normalized [Args].
func ParseArgs(passedArgs []string) Args {
	args := sets.New[string]()

	for _, arg := range passedArgs {
		// trim left and right spaces
		// trim spaces in between from multiple spaces to just one i.e. uid   1001 would turn into uid 1001
		// if there is a space between, replace it with an = sign
		arg = strings.Replace(strings.Join(strings.Fields(strings.Trim(arg, " ")), " "), " ", "=", -1)
		// prepend -- if it's not already there
		if !strings.HasPrefix(arg, "-") {
			arg = "--" + arg
		}

		// disallow options that don't make sense in CSI
		switch arg {
		case "--foreground", "-f", "--help", "-h", "--version", "-v":
			continue
		}

		args.Insert(arg)
	}

	return Args{args}
}

// ParsedArgs creates [Args] from already parsed arguments by [ParseArgs].
func ParsedArgs(parsedArgs []string) Args {
	return Args{args: sets.New(parsedArgs...)}
}

// Insert inserts given normalized argument to [Args] if not exists.
func (a *Args) Insert(arg string) {
	a.args.Insert(arg)
}

// Value extracts value of given key, it returns extracted value and whether the key was found.
func (a *Args) Value(key string) (string, bool) {
	_, val, exists := a.find(key)
	return val, exists
}

// Has returns whether given key exists in [Args].
func (a *Args) Has(key string) bool {
	_, _, exists := a.find(key)
	return exists
}

// Remove removes given key, it returns the key's value and whether the key was found.
func (a *Args) Remove(key string) (string, bool) {
	entry, val, exists := a.find(key)
	if exists {
		a.args.Delete(entry)
	}
	return val, exists
}

// SortedList returns ordered list of normalized arguments.
func (a *Args) SortedList() []string {
	return sets.List(a.args)
}

// find tries to find given key from [Args], and returns whole entry, value and whether the key was found.
func (a *Args) find(key string) (string, string, bool) {
	key, prefix := a.keysForSearch(key)

	for _, arg := range a.args.UnsortedList() {
		if key == arg {
			return key, "", true
		}

		if strings.HasPrefix(arg, prefix) {
			val := strings.SplitN(arg, "=", 2)[1]
			return arg, val, true
		}
	}

	return "", "", false
}

// keysForSearch returns whole key and a prefix to search for given key in [Args].
// First one is the whole key to look for without `=` at the end for option-like arguments without any value.
// Second one is a prefix with `=` at the end for arguments with value.
//
// Arguments are normalized to `--key[=value]` in [ParseArgs], here this function also makes sure
// the returned prefixes have the same prefix format for the given key.
func (a *Args) keysForSearch(key string) (string, string) {
	prefix := strings.TrimSuffix(key, "=")

	if !strings.HasPrefix(key, "-") {
		prefix = "--" + prefix
	}

	return prefix, prefix + "="
}
