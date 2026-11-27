package watcher

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"go.uber.org/zap"
)

// secretEnvPrefix is where ${secret.NAME} looks up its value today:
// the watcher process's QOOQ_SECRET_<NAME>.
//
// The backing store is deliberately trivial; the syntax is the point. A secret
// that belongs to the deployment rather than to one app's manifest has no way
// to be expressed except ${env.*}, which reads the watcher's whole environment
// and is therefore the exact thing that has to go away before one watcher can
// serve more than one user. Establishing ${secret.NAME} now means that when
// the store becomes a real per-namespace one, every manifest already written
// keeps working unchanged — and manifests, unlike code, live in other people's
// repositories where they cannot be migrated.
const secretEnvPrefix = "QOOQ_SECRET_"

// lookupSecret resolves ${secret.NAME}. Names are used verbatim: an env var is
// case-sensitive, and quietly upper-casing would make ${secret.apiKey} and
// ${secret.APIKEY} the same reference, which is the kind of surprise that only
// shows up in production.
func lookupSecret(name string) (string, bool) {
	if name == "" {
		return "", false
	}
	return os.LookupEnv(secretEnvPrefix + name)
}

// interpolateRe matches a ${...} reference. The body is deliberately
// restrictive (no nesting, no defaults, no shell syntax): this is a lookup
// into a known set of values, not an expression language.
var interpolateRe = regexp.MustCompile(`\$\{([a-zA-Z0-9_.\-]+)\}`)

// interpolationScope is everything a ${...} reference can resolve to during
// one deploy. It is built once per pipeline run and consulted for every value
// that supports interpolation.
//
// The namespaces:
//
//	${app} / ${component} / ${network} / ${commit}   this deploy
//	${host}                                          where this host is reachable
//	${service.<name>.host|port|user|...}             a declared service
//	${volume.<name>.target|host_path}                a declared volume
//	${input.NAME}                                    a value the manifest declared
//	${secret.NAME}                                   a named secret
//	${env.NAME}                                      the watcher's environment
//
// ${env.*} reads the *watcher process's* environment, not the app's. That lets
// a manifest reference a value the operator supplied without it ever being
// written into the repo — but it reads the whole environment, so on a watcher
// serving more than one user it is a hole rather than a feature. Prefer
// ${input.NAME} for values this app declares it needs, and ${secret.NAME} for
// deployment-level secrets; ${env.*} remains for the single-operator case.
type interpolationScope struct {
	values map[string]string
	// prefixes hold whole families of references whose names are not known in
	// advance ("component.<any name>.<field>"). Registered by the caller that
	// has the context they need — a qooqd client, the current namespace —
	// which the scope itself deliberately does not.
	prefixes map[string]func(rest string) (string, bool)
	// explain lets a prefix resolver say why a reference it owns did not
	// resolve. Without it every failure reads as a typo — and "unknown
	// reference (available: ...)" is actively misleading for a reference that
	// is spelled correctly and simply names something not deployed yet.
	explain func(ref string) (string, bool)
	// lazy holds keys whose value costs something to obtain (an HTTP call to
	// qooqd, say). They are resolved only if a manifest actually references
	// them, and at most once per scope.
	lazy map[string]func() (string, bool)
	// secrets resolves ${secret.*} and ${env.*}, bound to one namespace.
	//
	// A field rather than a package-level lookup, because the namespace is the
	// isolation: a scope built for one namespace has no way to express a
	// question about another. Nil means neither family resolves at all, which
	// is the safe direction for a path that forgot to set it.
	secrets *secretResolver
}

// newInterpolationScope builds a scope bound to one namespace's secrets.
//
// The resolver is a constructor argument rather than something to attach
// afterwards, so a new call site cannot forget it. That is not hypothetical:
// the first version of M3 made it a setter, and a path that missed it stopped
// resolving ${env.*} — a silent regression for existing single-operator
// installs, caught only because a test happened to cover it.
//
// secrets may be nil, and then neither ${secret.*} nor ${env.*} resolves. Nil
// is spelled out at the call site, which is the point: it is visible rather
// than omitted.
func newInterpolationScope(secrets *secretResolver) *interpolationScope {
	return &interpolationScope{
		values:   make(map[string]string),
		prefixes: make(map[string]func(string) (string, bool)),
		lazy:     make(map[string]func() (string, bool)),
		secrets:  secrets,
	}
}

// set registers one resolvable key (e.g. "service.db.host").
func (s *interpolationScope) set(key, value string) {
	s.values[key] = value
}

// setLazy registers a key whose value is only computed if referenced. fn
// returning false means the value could not be obtained, which surfaces as the
// ordinary unknown-reference error — a manifest asking for something we cannot
// supply must fail loudly, not deploy with a blank.
func (s *interpolationScope) setLazy(key string, fn func() (string, bool)) {
	s.lazy[key] = fn
}

// setPrefix registers a resolver for every reference starting with prefix
// (which must end in a dot). fn receives what follows it.
func (s *interpolationScope) setPrefix(prefix string, fn func(rest string) (string, bool)) {
	s.prefixes[prefix] = fn
}

// resolve looks a reference up. ${env.*} is resolved live rather than
// pre-loaded so the watcher's whole environment never has to be copied into
// the scope (and so an unset variable is distinguishable from an empty one).
func (s *interpolationScope) resolve(ref string) (string, bool) {
	if name, ok := strings.CutPrefix(ref, "secret."); ok {
		if s.secrets == nil {
			// No resolver means nothing may be resolved. Falling back to the
			// process environment here would reintroduce the exact hole M3
			// closes, on whichever path forgot to attach one.
			return "", false
		}
		return s.secrets.lookup(name)
	}
	if name, ok := strings.CutPrefix(ref, "env."); ok {
		if s.secrets == nil {
			return "", false
		}
		return s.secrets.lookupEnv(name)
	}
	for prefix, fn := range s.prefixes {
		if rest, ok := strings.CutPrefix(ref, prefix); ok {
			return fn(rest)
		}
	}
	if v, ok := s.values[ref]; ok {
		return v, true
	}
	if fn, ok := s.lazy[ref]; ok {
		v, ok := fn()
		if ok {
			// Memoize: a manifest may reference the same key many times, and
			// the cost of these is the reason they are lazy.
			s.values[ref] = v
		}
		return v, ok
	}
	return "", false
}

// expand replaces every ${...} in raw. An unknown reference is an error, never
// an empty string: silently expanding ${service.db.hst} to "" produces an app
// that starts and then fails to connect for reasons nothing explains. The
// error lists what was available, since a typo is the usual cause.
func (s *interpolationScope) expand(raw string) (string, error) {
	var firstErr error
	out := interpolateRe.ReplaceAllStringFunc(raw, func(match string) string {
		ref := interpolateRe.FindStringSubmatch(match)[1]
		v, ok := s.resolve(ref)
		if !ok {
			if firstErr == nil {
				if s.explain != nil {
					if why, ok := s.explain(ref); ok {
						firstErr = fmt.Errorf("${%s}: %s", ref, why)
						return match
					}
				}
				firstErr = fmt.Errorf("unknown reference ${%s} (available: %s)", ref, strings.Join(s.known(), ", "))
			}
			return match
		}
		return v
	})
	if firstErr != nil {
		return "", firstErr
	}
	return out, nil
}

// expandMap expands every value of m (keys are never interpolated: an
// environment variable's name is part of the app's contract, not a value).
func (s *interpolationScope) expandMap(m map[string]string) (map[string]string, error) {
	if len(m) == 0 {
		return m, nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		expanded, err := s.expand(v)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		out[k] = expanded
	}
	return out, nil
}

// expandSlice expands every element of in.
func (s *interpolationScope) expandSlice(in []string) ([]string, error) {
	if len(in) == 0 {
		return in, nil
	}
	out := make([]string, 0, len(in))
	for _, v := range in {
		expanded, err := s.expand(v)
		if err != nil {
			return nil, err
		}
		out = append(out, expanded)
	}
	return out, nil
}

// expandMounts expands the source and target of every mount declaration, so a
// path can reference e.g. ${env.HOME}.
func (s *interpolationScope) expandMounts(in []manifestMount) ([]manifestMount, error) {
	if len(in) == 0 {
		return in, nil
	}
	out := make([]manifestMount, 0, len(in))
	for _, m := range in {
		from, err := s.expand(m.From)
		if err != nil {
			return nil, err
		}
		to, err := s.expand(m.To)
		if err != nil {
			return nil, err
		}
		out = append(out, manifestMount{From: from, To: to, Mode: m.Mode})
	}
	return out, nil
}

// applyRunConfig resolves everything in the manifest's run: section that
// needs the deploy's own context — mounts (which must be materialized out of
// the clone), env files (which must be read), and ${...} references — and
// writes the results into spec.
//
// It runs as one step, after the build and before the deploy request, because
// these are interdependent: a volume must be materialized before
// ${volume.x.host_path} can resolve, and env values may reference it. Both the
// container and process pipelines call it, so a manifest means the same thing
// under either runtime.
//
// Env precedence, lowest to highest: values injected from declared services,
// then each env_file in order, then run.env. The operator's deploy_spec.env
// still wins over all of it later, in mergeDeploySpec.
func (w *Watcher) applyRunConfig(ctx context.Context, t *Target, mf *manifest, spec deploySpecFields, cloneDir, sha7 string) (out deploySpecFields, err error) {
	// The namespace comes from the target, not from spec: spec here is the
	// manifest-derived half, and the namespace lives in the operator's
	// deploy_spec, which is merged over it later. Reading spec.Namespace at
	// this point yields "" for every deployment there has ever been — and ""
	// never matches the "default" qooqd stores, so anything that looks a
	// component up by it silently finds nothing.
	namespace := w.targetNamespace(t)
	ref := componentRef{namespace: namespace, name: t.ComponentName}

	// Bound to this deploy's namespace: that binding is what stops one
	// namespace's manifest from reaching another's secrets.
	scope := newInterpolationScope(w.newSecretResolver(namespace))

	// ${component.<name>.<field>} addresses another component of this
	// deployment, so two apps that talk to each other do not each have to
	// hardcode the other's port. Hardcoding is what makes a port change break
	// something far away — and with "auto:" ports there is no number to
	// hardcode in the first place.
	//
	// A reference to a component that is not deployed yet is not a mistake; it
	// is an ordering problem that fixes itself. It is reported the same way a
	// missing input is — the deploy waits, and the next sweep tries again —
	// rather than as a failure, which would read as something being wrong with
	// this app.
	crossRef := &componentResolver{ctx: ctx, client: w.client, namespace: namespace}
	scope.setPrefix("component.", crossRef.resolve)
	// Two resolvers can explain a failure, so they are chained rather than one
	// overwriting the other. Without the chain, adding the secret explanation
	// would have silently removed the cross-reference one — and "unknown
	// reference" for a component that simply has not deployed yet is the exact
	// misleading message that explanation was added to fix.
	scope.explain = chainExplain(crossRef.explain, scope.secrets.explainUnresolved)
	// A deploy blocked only because something it names is not up yet is not a
	// failure; classify it the way a missing input is, so it waits and retries
	// instead of reading as broken. The message itself already says what it is
	// waiting for — see componentResolver.explain — so this only labels it.
	// hostErr is why ${host} could not be asked for. It surfaces as an
	// unknown reference, which reads like the manifest's mistake; when the
	// cause is qooqd or its runtime not answering, it is not.
	var hostErr error
	defer func() {
		if err != nil && crossRef.waitingFor() != "" {
			pending := crossRef.pendingNames()
			if cycle := w.waitCycle(t, namespace, pending); cycle != nil {
				err = &wordedError{err: err, msg: circularReferenceMessage(cycle, namespace)}
			}
			err = waitingOnComponents(err, pending)
		}
		if err != nil && hostErr != nil && isNotTheRepository(notTheDaemon(hostErr)) {
			// The interpolation error says "unknown reference ${host}" while
			// listing host as available — a contradiction that sends the
			// user to edit a manifest that is fine (E1). Say what happened.
			// Only when it is ${host} that failed: another reference's error
			// stays as it is, and is the repository's.
			if strings.Contains(err.Error(), "${host}") {
				err = &wordedError{err: err, msg: "${host} cannot be resolved right now: this server's address could not be asked for (the node is not answering); it is retried on its own"}
			}
			err = notTheRepository(err)
		}
	}()

	scope.set("app", mf.App)
	scope.set("namespace", namespace)
	scope.set("component", t.ComponentName)
	scope.set("network", spec.Network)
	scope.set("commit", sha7)

	// ${host} is the address other machines reach this deployment on. It comes
	// from qooqd because that is where the knowledge lives (qooqd publishes the
	// ports and knows its own advertise host), and it exists so a manifest can
	// write http://${host}:8080 for a browser-side base URL instead of a LAN IP
	// that is correct on exactly one machine.
	//
	// Lazy, so the overwhelming majority of manifests that never mention it
	// don't pay for an extra call; and resolved per deploy rather than cached
	// across them, because a laptop that changed networks has a different
	// answer and a stale-but-plausible address is the expensive kind of wrong.
	// If qooqd can't be asked, ${host} stays unresolvable and a manifest using
	// it fails with the usual unknown-reference error — better than deploying
	// with a silently wrong address.
	scope.setLazy("host", func() (string, bool) {
		capResp, err := w.client.Capacity(ctx)
		if err != nil {
			hostErr = err
			w.log.Warn("capacity unavailable, ${host} cannot resolve", zap.String("target", t.Name), zap.Error(err))
			return "", false
		}
		return capResp.AdvertiseHost, capResp.AdvertiseHost != ""
	})

	// ${input.NAME} for everything the manifest declares. Registered after
	// ${host} so an input can default to it, and before services so a service
	// password can come from one. runPipeline has already established that no
	// required input is missing.
	inputVals, err := w.loadInputValues(t)
	if err != nil {
		return spec, fmt.Errorf("inputs: %w", err)
	}
	if err := applyInputs(scope, mf.Inputs, inputVals); err != nil {
		return spec, err
	}

	// Services first: they must be running, and their connection details
	// registered in the scope, before anything that might reference them.
	injected, deployedServices, err := w.applyServicesTopology(ctx, t, mf, namespace, spec.Network, scope)
	if err != nil {
		return spec, err
	}

	files, err := scope.expandMounts(mf.Run.Files)
	if err != nil {
		return spec, fmt.Errorf("run.files: %w", err)
	}
	volumes, err := scope.expandMounts(mf.Run.Volumes)
	if err != nil {
		return spec, fmt.Errorf("run.volumes: %w", err)
	}

	mounts, err := w.materializeMounts(ref, cloneDir, sha7, files, volumes)
	if err != nil {
		return spec, err
	}
	spec.Mounts = mounts

	// Volumes become addressable only once materialized: their host path is
	// derived, not declared. materializeMounts emits files first, then
	// volumes, in declaration order.
	for i, v := range volumes {
		resolved := mounts[len(files)+i]
		scope.set("volume."+v.From+".target", resolved.ContainerPath)
		scope.set("volume."+v.From+".host_path", resolved.HostPath)
	}

	envFileEntries, err := scope.expandSlice(mf.Run.EnvFile)
	if err != nil {
		return spec, fmt.Errorf("run.env_file: %w", err)
	}
	fromFiles, err := w.resolveEnvFiles(ref, cloneDir, envFileEntries)
	if err != nil {
		return spec, err
	}
	runEnv, err := scope.expandMap(mf.Run.Env)
	if err != nil {
		return spec, fmt.Errorf("run.env: %w", err)
	}

	merged := make(map[string]string, len(injected)+len(fromFiles)+len(runEnv))
	for _, layer := range []map[string]string{injected, fromFiles, runEnv} {
		for k, v := range layer {
			merged[k] = v
		}
	}
	if len(merged) > 0 {
		spec.Env = merged
	}

	command, err := scope.expandSlice(mf.Run.Command)
	if err != nil {
		return spec, fmt.Errorf("run.command: %w", err)
	}
	spec.Command = command

	// Everything resolved: record what this deploy is connected to, so the
	// row describes the deploy that is about to be marked successful.
	t.Topology = encodeTopology(deployedServices, crossRef.references())

	return spec, nil
}

// known lists the resolvable keys, sorted, for error messages. ${env.*} is
// summarized rather than enumerated — dumping the watcher's entire environment
// into an error message would be both useless and a disclosure risk.
func (s *interpolationScope) known() []string {
	out := make([]string, 0, len(s.values)+len(s.lazy)+1)
	for k := range s.values {
		out = append(out, k)
	}
	for k := range s.lazy {
		if _, alreadyResolved := s.values[k]; !alreadyResolved {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	// Summarized rather than enumerated: dumping the watcher's entire
	// environment into an error message would be both useless and a
	// disclosure risk.
	return append(out, "secret.<NAME>", "env.<NAME>")
}

// chainExplain returns the first explanation any of fns can give.
func chainExplain(fns ...func(string) (string, bool)) func(string) (string, bool) {
	return func(ref string) (string, bool) {
		for _, fn := range fns {
			if fn == nil {
				continue
			}
			if why, ok := fn(ref); ok {
				return why, true
			}
		}
		return "", false
	}
}
