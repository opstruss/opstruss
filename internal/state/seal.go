package state

import (
	"fmt"
	"maps"
	"slices"
)

// The parts of a record and of an attempt that keep sensitive values, as
// Hidden and Sensitive name them.
const (
	PartInputs    = "inputs"
	PartOutputs   = "outputs"
	PartEnv       = "destroy.env"
	PartEnvFiles  = "destroy.env_files"
	PartRawOutput = "raw_output" // OPSTRUSS_OUTPUT as written, encrypted as a whole
)

// KeyError is a record that needs the data key of its stack, which could
// not be had.
type KeyError struct{ Err error }

func (e *KeyError) Error() string { return "the data key of the stack: " + e.Err.Error() }
func (e *KeyError) Unwrap() error { return e.Err }

// parts points at what a record or an attempt keeps values in.
type parts struct {
	inputs, outputs *map[string]any
	destroy         **Destroy
	raw             *string
	hidden          *map[string][]string
	sensitive       *map[string]map[string]string
}

func (r *Record) parts() parts {
	return parts{&r.Inputs, &r.Outputs, &r.Destroy, nil, &r.Hidden, &r.Sensitive}
}

func (a *Attempt) parts() parts {
	return parts{&a.Inputs, &a.Outputs, &a.Destroy, &a.RawOutput, &a.Hidden, &a.Sensitive}
}

// sealer encrypts and decrypts the sensitive values of one record, asking
// for the data key only when a value needs it.
type sealer struct {
	key    func() ([]byte, error)
	known  func(part, name string, v any) bool
	opened func(v any)
	id     string
	k      []byte
}

func (s *sealer) dataKey() ([]byte, error) {
	if s.k != nil {
		return s.k, nil
	}
	if s.key == nil {
		return nil, &KeyError{Err: fmt.Errorf("nothing opens it here")}
	}
	k, err := s.key()
	if err != nil {
		return nil, &KeyError{Err: err}
	}
	s.k = k
	return k, nil
}

// plain is the values of a part, as values.
func (p parts) plain(part string) map[string]any {
	switch part {
	case PartInputs:
		return *p.inputs
	case PartOutputs:
		return *p.outputs
	}
	if *p.destroy == nil {
		return nil
	}
	env := (*p.destroy).Env
	if part == PartEnvFiles {
		env = (*p.destroy).EnvFiles
	}
	out := map[string]any{}
	for k, v := range env {
		out[k] = v
	}
	return out
}

// seal moves the values that Hidden names, and those that known finds to
// hold a sensitive value, into Sensitive, encrypted. p points into a copy
// whose maps it may change.
func (s *sealer) seal(p parts) error {
	hidden := map[string][]string{}
	for part, names := range *p.hidden {
		hidden[part] = slices.Clone(names)
	}
	for _, part := range []string{PartInputs, PartOutputs, PartEnv, PartEnvFiles} {
		for _, k := range slices.Sorted(maps.Keys(p.plain(part))) {
			if !slices.Contains(hidden[part], k) && s.known != nil && s.known(part, k, p.plain(part)[k]) {
				hidden[part] = append(hidden[part], k)
			}
		}
	}
	// OPSTRUSS_OUTPUT as written is encrypted whole when the attempt has
	// any sensitive value, or Hidden names it.
	_, rawHidden := hidden[PartRawOutput]
	delete(hidden, PartRawOutput)
	sealRaw := p.raw != nil && *p.raw != "" && !IsSealed(*p.raw) &&
		(rawHidden || len(hidden) > 0 || s.known != nil && s.known(PartRawOutput, "", *p.raw))
	*p.hidden = nil
	if len(hidden) == 0 && !sealRaw {
		return nil
	}
	key, err := s.dataKey()
	if err != nil {
		return err
	}
	sens := map[string]map[string]string{}
	for part, vals := range *p.sensitive {
		sens[part] = maps.Clone(vals)
	}
	if sealRaw {
		if *p.raw, err = Seal(key, s.id, *p.raw); err != nil {
			return err
		}
	}
	if *p.destroy != nil {
		d := **p.destroy
		d.Env, d.EnvFiles = maps.Clone(d.Env), maps.Clone(d.EnvFiles)
		*p.destroy = &d
	}
	*p.inputs, *p.outputs = maps.Clone(*p.inputs), maps.Clone(*p.outputs)
	for part, names := range hidden {
		values := p.plain(part)
		for _, name := range names {
			v, ok := values[name]
			if !ok {
				continue
			}
			ct, err := Seal(key, s.id, v)
			if err != nil {
				return err
			}
			if sens[part] == nil {
				sens[part] = map[string]string{}
			}
			sens[part][name] = ct
			switch part {
			case PartInputs:
				delete(*p.inputs, name)
			case PartOutputs:
				delete(*p.outputs, name)
			case PartEnv:
				delete((*p.destroy).Env, name)
			case PartEnvFiles:
				delete((*p.destroy).EnvFiles, name)
			}
		}
	}
	*p.sensitive = sens
	return nil
}

// open puts the values in Sensitive back in their parts, decrypted, and
// names them in Hidden.
func (s *sealer) open(p parts) error {
	if len(*p.sensitive) == 0 && (p.raw == nil || !IsSealed(*p.raw)) {
		return nil
	}
	key, err := s.dataKey()
	if err != nil {
		return err
	}
	hidden := map[string][]string{}
	if p.raw != nil && IsSealed(*p.raw) {
		v, err := Unseal(key, s.id, *p.raw)
		if err != nil {
			return fmt.Errorf("raw_output: %w", err)
		}
		raw, _ := v.(string)
		*p.raw = raw
		hidden[PartRawOutput] = nil
	}
	for _, part := range slices.Sorted(maps.Keys(*p.sensitive)) {
		vals := (*p.sensitive)[part]
		for _, name := range slices.Sorted(maps.Keys(vals)) {
			v, err := Unseal(key, s.id, vals[name])
			if err != nil {
				return fmt.Errorf("sensitive.%s.%s: %w", part, name, err)
			}
			if s.opened != nil {
				s.opened(v)
			}
			switch part {
			case PartInputs, PartOutputs:
				m := p.inputs
				if part == PartOutputs {
					m = p.outputs
				}
				if *m == nil {
					*m = map[string]any{}
				}
				(*m)[name] = v
			case PartEnv, PartEnvFiles:
				if *p.destroy == nil {
					*p.destroy = &Destroy{}
				}
				text, _ := v.(string)
				env := &(*p.destroy).Env
				if part == PartEnvFiles {
					env = &(*p.destroy).EnvFiles
				}
				if *env == nil {
					*env = map[string]string{}
				}
				(*env)[name] = text
			default:
				return fmt.Errorf("sensitive.%s: a part this OpsTruss does not know", part)
			}
			hidden[part] = append(hidden[part], name)
		}
	}
	*p.sensitive, *p.hidden = nil, hidden
	return nil
}

// sealRecord is a copy of r with its sensitive values, and those of its
// attempts, encrypted.
func (s *Store) sealRecord(r *Record) (*Record, error) {
	sl := &sealer{key: s.Key, known: s.Known, id: r.ID}
	cp := *r
	if err := sl.seal(cp.parts()); err != nil {
		return nil, err
	}
	if r.Pending != nil {
		a := *r.Pending
		if err := sl.seal(a.parts()); err != nil {
			return nil, err
		}
		cp.Pending = &a
	}
	cp.Abandoned = nil
	for _, old := range r.Abandoned {
		a := *old
		if err := sl.seal(a.parts()); err != nil {
			return nil, err
		}
		cp.Abandoned = append(cp.Abandoned, &a)
	}
	return &cp, nil
}

// openRecord decrypts the sensitive values of r and of its attempts in
// place, when the store has a way to the data key; without one, as for
// opstruss state, they stay encrypted.
func (s *Store) openRecord(r *Record) error {
	if s.Key == nil {
		return nil
	}
	sl := &sealer{key: s.Key, opened: s.Opened, id: r.ID}
	if err := sl.open(r.parts()); err != nil {
		return err
	}
	if r.Pending != nil {
		if err := sl.open(r.Pending.parts()); err != nil {
			return err
		}
	}
	for _, a := range r.Abandoned {
		if err := sl.open(a.parts()); err != nil {
			return err
		}
	}
	return nil
}
