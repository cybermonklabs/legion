package config

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/sjawhar/legion/daemon/internal/ompsessions"
)

// answersKey is the `answers` block; conversationStoreKey its connection to the conversation store.
const (
	answersKey           = "answers"
	conversationStoreKey = answersKey + ".conversation_store"
)

// AnswerProvider is a model provider the answer loop calls, a key of `answers.providers`.
type AnswerProvider string

const (
	// AnswerAnthropic is Anthropic's Messages API.
	AnswerAnthropic AnswerProvider = "anthropic"
	// AnswerOpenAI is OpenAI's Responses API.
	AnswerOpenAI AnswerProvider = "openai"
)

// answerProviders are the providers `answers.providers` may name, in the order they are read.
var answerProviders = []string{string(AnswerAnthropic), string(AnswerOpenAI)}

// answerModelID is a model as a conversation names it, `<provider>/<model>`, the key of
// `answers.prices`; the provider is one the loop calls.
var answerModelID = regexp.MustCompile(`^(anthropic|openai)/\S+$`)

// Answers is the settled `answers` block: what the daemon's model loop needs to answer a question
// to an agent that is not running, from that agent's own conversation. Config.Answers is nil when
// the file sets none, or sets one that names no model provider (answersOffWithNoProvider) or no
// connection to the conversation store its runtime.kubernetes.session_store reads through
// (answersOff), and the loop is then off.
type Answers struct {
	// Providers are the model providers the loop may call, each its endpoint and its key.
	Providers map[AnswerProvider]AnswerEndpoint
	// Prices are USD per million tokens, by model as the conversation names it
	// (`anthropic/claude-opus-5-5`); a model with no price is never answered on.
	Prices map[string]AnswerPrice
	// ConversationStore is the daemon's own connection to the database the conversations live in.
	ConversationStore ConversationStore
	// Bounds are the loop's limits, each the file's or its default.
	Bounds AnswerBounds
}

// AnswerEndpoint is one `answers.providers.<provider>` entry: the base URL the provider's API is
// called at, and the key it is called with.
type AnswerEndpoint struct {
	BaseURL    string
	Credential AnswerCredential
}

// AnswerCredential is a provider's key as the file names it: exactly one of File, a path resolved
// against the file's directory (`api_key_file`), and Command, a command that prints the key
// (`api_key_command`). Nothing reads it at load or at boot: Read reads it again for every model
// call, so a key the operator rotates, or a short-lived token a command mints, is the one each call
// carries.
type AnswerCredential struct {
	File    string
	Command string
	// key is the provider's key in the file (`answers.providers.anthropic`), which every refusal
	// names.
	key string
}

// Read is the provider's key now: the file read again, or the command run again (under sh, with
// the daemon's environment less its NATS seeds, ended with ctx), its surrounding whitespace
// trimmed. An unreadable or empty file, and a command that fails or prints nothing, are refusals
// naming the key and the file or the command's exit and stderr; no refusal and no log line carries
// the key itself.
func (c AnswerCredential) Read(ctx context.Context) (string, error) {
	switch {
	case c.File != "":
		return ReadSecretPointer(c.key+".api_key_file", c.File)
	case c.Command != "":
		return runKeyCommand(ctx, c.Command, c.key+".api_key_command")
	default:
		return "", fmt.Errorf("%s names neither api_key_file nor api_key_command", c.key)
	}
}

// AnswerPrice is one model's `answers.prices` entry, each USD per million tokens: input read
// fresh, output, input written to the provider's cache, and input read from it.
type AnswerPrice struct {
	Input      float64
	Output     float64
	CacheWrite float64
	CacheRead  float64
}

// ConversationStore is the settled `answers.conversation_store`: the daemon's own way into the
// database the deployment's conversations live in. Under runtime.kubernetes.session_store postgres,
// DSNFile is the file holding that database's postgres:// URL, which boot reads; under
// runtime.tmux.session_store postgres it is runtime.tmux.session_dsn_file, the file the daemon
// shares with its panes. Under cloudsql-iam, Database is the instance and database
// runtime.kubernetes.session_database names with the IAM login the daemon signs in as. Both are
// empty under runtime tmux's files store, whose conversations are files on the daemon's own host.
type ConversationStore struct {
	DSNFile  string
	Database ompsessions.Database
}

// AnswerBounds are `answers.bounds`: how long one answer may take, how many model calls it may make
// and how many tokens each may write; how long one `gh` call may run, how much of its output the
// model reads, and how many calls one answer may make; how many questions the project answers at
// once; and how many USD the project spends a UTC day.
type AnswerBounds struct {
	Answer        time.Duration
	ModelCalls    int
	OutputTokens  int
	GHCall        time.Duration
	GHOutputChars int
	GHCalls       int
	Concurrent    int
	DailyUSD      float64
}

// defaultAnswerBounds are the bounds a file that sets none runs on.
func defaultAnswerBounds() AnswerBounds {
	return AnswerBounds{
		Answer: 180 * time.Second, ModelCalls: 8, OutputTokens: 4000, GHCall: 30 * time.Second,
		GHOutputChars: 60000, GHCalls: 20, Concurrent: 4, DailyUSD: 50,
	}
}

// answersFile is `answers` as the file wrote it: everything but what needs the runtime (which
// conversation_store key applies) and the file's directory (where api_key_file is). providers is
// empty for a block that names no provider, which leaves the loop off (resolveAnswers).
type answersFile struct {
	providers map[AnswerProvider]AnswerEndpoint
	prices    map[string]AnswerPrice
	bounds    AnswerBounds
	// storeSet is whether the file sets conversation_store at all; dsnFile and user are its keys,
	// "" when unset.
	storeSet      bool
	dsnFile, user string
}

// readAnswers reads `answers`; nil for an unset or null block, which leaves the loop off.
func readAnswers(value *yaml.Node, key string) (*answersFile, error) {
	if value.Tag == "!!null" {
		return nil, nil
	}
	if value.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s must be a mapping", key)
	}
	fields, err := describedMembers(value, key, answersKey)
	if err != nil {
		return nil, err
	}
	read := &answersFile{}
	if read.providers, err = readAnswerProviders(fields["providers"], key+".providers"); err != nil {
		return nil, err
	}
	if read.prices, err = readAnswerPrices(fields["prices"], key+".prices", read.providers); err != nil {
		return nil, err
	}
	if store := fields["conversation_store"]; store != nil {
		read.storeSet = true
		if read.dsnFile, read.user, err = readConversationStore(store, key+".conversation_store"); err != nil {
			return nil, err
		}
	}
	if read.bounds, err = readAnswerBounds(fields["bounds"], key+".bounds"); err != nil {
		return nil, err
	}
	return read, nil
}

// readAnswerProviders reads `answers.providers`: anthropic, openai or both. An absent or null key
// and an empty mapping name none, which answers no provider and leaves the loop off
// (resolveAnswers); a mapping that names a provider and gives none of them an endpoint is refused.
func readAnswerProviders(value *yaml.Node, key string) (map[AnswerProvider]AnswerEndpoint, error) {
	if value == nil {
		return nil, nil
	}
	if value.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s must be a mapping of provider (anthropic, openai) to its base_url and key", key)
	}
	if len(value.Content) == 0 {
		return nil, nil
	}
	named, err := describedMembers(value, key, answersKey+".providers")
	if err != nil {
		return nil, err
	}
	if len(named) == 0 {
		return nil, fmt.Errorf("%s names no provider with an endpoint: give anthropic or openai its base_url and its key, or remove the key to leave the loop off", key)
	}
	providers := make(map[AnswerProvider]AnswerEndpoint, len(named))
	for _, name := range answerProviders {
		node, ok := named[name]
		if !ok {
			continue
		}
		endpoint, err := readAnswerEndpoint(node, key+"."+name)
		if err != nil {
			return nil, err
		}
		providers[AnswerProvider(name)] = endpoint
	}
	return providers, nil
}

// readAnswerEndpoint reads one provider: its base_url, and exactly one of api_key_file and
// api_key_command, the GitHub App's private_key_command pattern.
func readAnswerEndpoint(value *yaml.Node, key string) (AnswerEndpoint, error) {
	if value.Kind != yaml.MappingNode {
		return AnswerEndpoint{}, fmt.Errorf("%s must be a mapping", key)
	}
	fields, err := describedMembers(value, key, answersKey+".providers.<PROVIDER>")
	if err != nil {
		return AnswerEndpoint{}, err
	}
	raw, err := requiredString(fields["base_url"], key+".base_url", ": the provider's API endpoint, an http or https URL")
	if err != nil {
		return AnswerEndpoint{}, err
	}
	base, err := httpBase(raw, key+".base_url")
	if err != nil {
		return AnswerEndpoint{}, err
	}
	credential := AnswerCredential{key: key}
	if credential.File, err = optionalString(fields["api_key_file"], key+".api_key_file"); err != nil {
		return AnswerEndpoint{}, err
	}
	if credential.Command, err = optionalString(fields["api_key_command"], key+".api_key_command"); err != nil {
		return AnswerEndpoint{}, err
	}
	if (credential.File == "") == (credential.Command == "") {
		return AnswerEndpoint{}, fmt.Errorf("%s requires exactly one of api_key_file or api_key_command", key)
	}
	return AnswerEndpoint{BaseURL: base, Credential: credential}, nil
}

// readAnswerPrices reads `answers.prices`: at least one model, each a model of a provider
// `answers.providers` names, priced in full. A block that names no provider has nothing to price,
// so the key may then be absent, null or empty; a model it does price is refused, its provider
// being one the block does not name.
func readAnswerPrices(value *yaml.Node, key string, providers map[AnswerProvider]AnswerEndpoint) (map[string]AnswerPrice, error) {
	if value == nil {
		if len(providers) == 0 {
			return nil, nil
		}
		return nil, fmt.Errorf("%s is required: the USD price per million tokens of each model the loop may answer on, by the id its conversation names (<provider>/<model>)", key)
	}
	if value.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s must be a mapping of model id (<provider>/<model>) to its prices", key)
	}
	if len(value.Content) == 0 {
		if len(providers) == 0 {
			return nil, nil
		}
		return nil, fmt.Errorf("%s must price at least one model", key)
	}
	prices := make(map[string]AnswerPrice, len(value.Content)/2)
	for i := 0; i+1 < len(value.Content); i += 2 {
		idNode, priceNode := value.Content[i], value.Content[i+1]
		id := idNode.Value
		match := answerModelID.FindStringSubmatch(id)
		if idNode.Kind != yaml.ScalarNode || match == nil {
			return nil, fmt.Errorf("%s names %q, which is no model id: <provider>/<model>, its provider anthropic or openai", key, id)
		}
		if _, seen := prices[id]; seen {
			return nil, fmt.Errorf("%s names %s twice", key, id)
		}
		entry := key + "." + id
		if _, ok := providers[AnswerProvider(match[1])]; !ok {
			return nil, fmt.Errorf("%s prices a model of %s, which %s.providers does not name, so the loop never calls it; remove it or configure the provider", entry, match[1], answersKey)
		}
		price, err := readAnswerPrice(priceNode, entry)
		if err != nil {
			return nil, err
		}
		prices[id] = price
	}
	return prices, nil
}

// readAnswerPrice reads one model's prices, every one of the four required: a price left out
// would be a cost the daily limit never counts.
func readAnswerPrice(value *yaml.Node, key string) (AnswerPrice, error) {
	if value.Kind != yaml.MappingNode {
		return AnswerPrice{}, fmt.Errorf("%s must be a mapping of input, output, cache_write and cache_read", key)
	}
	fields, err := describedMembers(value, key, answersKey+".prices.<MODEL>")
	if err != nil {
		return AnswerPrice{}, err
	}
	var price AnswerPrice
	for _, field := range []struct {
		name   string
		target *float64
	}{
		{"input", &price.Input}, {"output", &price.Output},
		{"cache_write", &price.CacheWrite}, {"cache_read", &price.CacheRead},
	} {
		read, err := readNumber(fields[field.name], key+"."+field.name)
		if err != nil {
			return AnswerPrice{}, err
		}
		if read == nil {
			return AnswerPrice{}, fmt.Errorf("%s.%s is required: USD per million tokens, 0 when the provider charges nothing for it", key, field.name)
		}
		if *read < 0 {
			return AnswerPrice{}, fmt.Errorf("%s.%s must be a non-negative number", key, field.name)
		}
		*field.target = *read
	}
	return price, nil
}

// readConversationStore reads `answers.conversation_store`'s two keys; which one the runtime's
// store needs is resolveAnswers' to say.
func readConversationStore(value *yaml.Node, key string) (dsnFile, user string, err error) {
	if value.Kind != yaml.MappingNode {
		return "", "", fmt.Errorf("%s must be a mapping", key)
	}
	fields, err := describedMembers(value, key, conversationStoreKey)
	if err != nil {
		return "", "", err
	}
	if dsnFile, err = optionalString(fields["dsn_file"], key+".dsn_file"); err != nil {
		return "", "", err
	}
	if user, err = optionalString(fields["user"], key+".user"); err != nil {
		return "", "", err
	}
	return dsnFile, user, nil
}

// readAnswerBounds reads `answers.bounds` over the defaults: each count a positive integer, each
// duration a positive number of seconds within the timer bound, daily_usd a positive number.
func readAnswerBounds(value *yaml.Node, key string) (AnswerBounds, error) {
	bounds := defaultAnswerBounds()
	if value == nil {
		return bounds, nil
	}
	if value.Kind != yaml.MappingNode {
		return AnswerBounds{}, fmt.Errorf("%s must be a mapping", key)
	}
	fields, err := describedMembers(value, key, answersKey+".bounds")
	if err != nil {
		return AnswerBounds{}, err
	}
	for _, bound := range []struct {
		name string
		max  int
		set  func(int)
	}{
		{"answer_seconds", maxTimerSeconds, func(n int) { bounds.Answer = time.Duration(n) * time.Second }},
		{"model_calls", 0, func(n int) { bounds.ModelCalls = n }},
		{"output_tokens", 0, func(n int) { bounds.OutputTokens = n }},
		{"gh_seconds", maxTimerSeconds, func(n int) { bounds.GHCall = time.Duration(n) * time.Second }},
		{"gh_output_chars", 0, func(n int) { bounds.GHOutputChars = n }},
		{"gh_calls", 0, func(n int) { bounds.GHCalls = n }},
		{"concurrent", 0, func(n int) { bounds.Concurrent = n }},
	} {
		read, err := readPositiveInteger(fields[bound.name], key+"."+bound.name, bound.max)
		if err != nil {
			return AnswerBounds{}, err
		}
		if read != nil {
			bound.set(*read)
		}
	}
	daily, err := readNumber(fields["daily_usd"], key+".daily_usd")
	if err != nil {
		return AnswerBounds{}, err
	}
	if daily != nil {
		if *daily <= 0 {
			return AnswerBounds{}, fmt.Errorf("%s.daily_usd must be a positive number", key)
		}
		bounds.DailyUSD = *daily
	}
	return bounds, nil
}

// readNumber reads a finite number, an integer or a decimal; nil for an unset key.
func readNumber(value *yaml.Node, key string) (*float64, error) {
	if value == nil || value.Tag == "!!null" {
		return nil, nil
	}
	var number float64
	if (value.Tag != "!!int" && value.Tag != "!!float") || value.Decode(&number) != nil || math.IsNaN(number) || math.IsInf(number, 0) {
		return nil, fmt.Errorf("%s must be a number", key)
	}
	return &number, nil
}

// resolveAnswers settles `answers` once the runtime is: each api_key_file against the file's
// directory, and the conversation store the runtime's own store needs (conversationStoreOf). A
// block that names no model provider, or no such store, leaves cfg.Answers nil, the loop off as
// with no block, and adds one line to cfg.Warnings saying why: the provider line when both are
// missing, since without a provider no store would turn the loop on. Every other key of the block
// was already read and held to its rules, and a conversation_store key the runtime does not use is
// refused before either line, so a malformed block is refused, never turned off.
func resolveAnswers(file fileConfig, configDir string, cfg *Config) error {
	read := file.Answers
	if read == nil {
		return nil
	}
	store, off, err := conversationStoreOf(read, configDir, cfg.Runtime)
	if err != nil {
		return err
	}
	if len(read.providers) == 0 {
		// With no connection to the conversation store either, the line names both things to
		// set, so an operator who follows it meets no second line at the next boot.
		if off != "" {
			off = answersOffWithNoProvider + answersOffAndNoStore
		} else {
			off = answersOffWithNoProvider
		}
	}
	if off != "" {
		cfg.Warnings = append(cfg.Warnings, off)
		return nil
	}
	answers := &Answers{
		Providers:         make(map[AnswerProvider]AnswerEndpoint, len(read.providers)),
		Prices:            read.prices,
		ConversationStore: store,
		Bounds:            read.bounds,
	}
	for provider, endpoint := range read.providers {
		if endpoint.Credential.File != "" {
			endpoint.Credential.File = underConfig(endpoint.Credential.File, configDir)
		}
		answers.Providers[provider] = endpoint
	}
	cfg.Answers = answers
	return nil
}

// conversationStoreOf is the connection the daemon reads conversations through, given where the
// runtime keeps them: a postgres:// URL's file under session_store postgres, the file the panes
// read under runtime tmux's (runtime.tmux.session_dsn_file); its own IAM login on
// session_database's instance under cloudsql-iam; none under runtime tmux's files store, whose
// conversations are files on the daemon's own host. Each store refuses the key it does not use, as
// runtime.kubernetes' session keys do; under runtime tmux the block names nothing, since the daemon
// reads the conversations where its panes keep them. A database store whose own key is unset
// answers off, the warning that the loop is off (answersOff), in place of a store: a deployment
// that names no connection runs as one without `answers`.
func conversationStoreOf(read *answersFile, configDir string, rt Runtime) (store ConversationStore, off string, err error) {
	k := rt.Kubernetes
	if k == nil {
		if read.storeSet {
			if rt.Tmux.InDatabase() {
				return ConversationStore{}, "", fmt.Errorf("%s is not used under runtime tmux: the daemon reads every conversation from the database %s names, where its panes keep them; remove it", conversationStoreKey, tmuxSessionDSNFileKey)
			}
			return ConversationStore{}, "", fmt.Errorf("%s is not used under runtime tmux: every conversation is a file on the daemon's own host, which it reads where the pane wrote it; remove it", conversationStoreKey)
		}
		return ConversationStore{DSNFile: rt.Tmux.SessionDSN()}, "", nil
	}
	switch kind := k.SessionStore.Kind; kind {
	case ompsessions.StorePostgres:
		if read.user != "" {
			return ConversationStore{}, "", inertSessionKey(conversationStoreKey+".user", kind)
		}
		if read.dsnFile == "" {
			return ConversationStore{}, answersOff(conversationStoreKey+".dsn_file", kind,
				"the file holding the postgres:// URL of the database the conversations live in"), nil
		}
		return ConversationStore{DSNFile: underConfig(read.dsnFile, configDir)}, "", nil
	case ompsessions.StoreCloudSQLIAM:
		if read.dsnFile != "" {
			return ConversationStore{}, "", inertSessionKey(conversationStoreKey+".dsn_file", kind)
		}
		if read.user == "" {
			return ConversationStore{}, answersOff(conversationStoreKey+".user", kind,
				"the IAM login the daemon signs in to "+sessionDatabaseKey+"'s instance as"), nil
		}
		database := k.SessionStore.Database
		database.User = read.user
		return ConversationStore{Database: database}, "", nil
	default:
		// No file reaches this: readSessionStore admits only the two database stores.
		return ConversationStore{}, "", fmt.Errorf("%s cannot read conversations under %s %q", answersKey, sessionStoreKey, kind)
	}
}

// answersOff is the warning for a file that sets `answers` and leaves key unset, the one
// connection to the conversation store its session store kind reads through. It says the loop is
// off, that a message to a stopped agent is refused and dropped as with no block, and that setting
// key to what, the value it names, turns the loop on. It is a warning and not a refusal so that a
// deployment that sets `answers` before it has that connection still boots as it does without the
// block.
func answersOff(key string, kind ompsessions.StoreKind, what string) string {
	return fmt.Sprintf("%s is set, but the answer loop is off: under %s %s, %s names the daemon's connection to the conversation store, and the file sets none, so a message to a stopped agent is refused and dropped as it is without %s; to turn the loop on, set %s to %s",
		answersKey, sessionStoreKey, kind, key, answersKey, key, what)
}

// answersOffWithNoProvider is the warning for a file that sets `answers` and names no model
// provider, its `providers` absent, null or empty: the loop is off, a message to a stopped agent is
// refused and dropped as with no block, and what turns the loop on. A warning, not a refusal, for
// the reason answersOff gives: such a deployment boots as it does without the block.
const answersOffWithNoProvider = answersKey + " is set, but the answer loop is off: " + answersKey +
	".providers names no model provider the loop may call, so a message to a stopped agent is refused and dropped as it is without " +
	answersKey + "; to turn the loop on, set " + answersKey + ".providers to at least one provider (anthropic, openai) with its base_url and its key, and " +
	answersKey + ".prices to the price of each model the loop may answer on"

// answersOffAndNoStore ends answersOffWithNoProvider for a Kubernetes file that names no
// connection to the conversation store either.
const answersOffAndNoStore = ", and " + conversationStoreKey + " to the daemon's connection to the conversation store as well"
