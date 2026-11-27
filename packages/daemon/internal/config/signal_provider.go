package config

import (
	"fmt"
	"regexp"
	"strconv"

	"gopkg.in/yaml.v3"
)

// SignalProvider is a project's model provider as the provider's headroom signal is configured:
// the uncached input tokens of a minute over the project's own limit, and which models count
// toward it. Each field is optional, and its zero value is the key unset.
type SignalProvider struct {
	// TokenLimit is `signal_token_limit_per_minute`, the uncached input tokens the project may
	// spend in a minute, and 0 where the file names none: the headroom then reads as not
	// recorded, never as 0.
	TokenLimit int64
	// TokenModels is `signal_token_model_pattern`, a Go regular expression of the models whose
	// tokens count against the limit; "" where the file names none, under which every model's
	// tokens count. A request of another model is kept all the same, and its tokens count nowhere.
	TokenModels string
}

// readSignalProvider reads a project's two provider keys out of fields, its members, under key,
// the project's own path. A key the project leaves out keeps its zero value.
func readSignalProvider(fields map[string]*yaml.Node, key string) (SignalProvider, error) {
	var provider SignalProvider
	if limit := fields["signal_token_limit_per_minute"]; limit != nil {
		read, err := readPositiveInteger(limit, key+".signal_token_limit_per_minute", 0)
		if err != nil {
			return SignalProvider{}, err
		}
		provider.TokenLimit = int64(*read)
	}
	if value := fields["signal_token_model_pattern"]; value != nil {
		read, err := readModelPattern(value, key+".signal_token_model_pattern")
		if err != nil {
			return SignalProvider{}, err
		}
		provider.TokenModels = read
	}
	return provider, nil
}

// readModelPattern is the model pattern key's value, a Go regular expression, refused naming the
// key and the reason where it is empty, which would count every model as leaving the key out does,
// or where Go's regexp refuses it.
func readModelPattern(value *yaml.Node, key string) (string, error) {
	pattern, err := stringOf(value, key)
	if err != nil {
		return "", err
	}
	if pattern == "" {
		return "", fmt.Errorf("%s must not be empty: an empty pattern matches every model, as leaving the key out does", key)
	}
	if _, err := regexp.Compile(pattern); err != nil {
		return "", fmt.Errorf("%s %s is not a Go regular expression: %w", key, strconv.Quote(pattern), err)
	}
	return pattern, nil
}
