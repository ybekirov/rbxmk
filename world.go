package rbxmk

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/robloxapi/types"
	"github.com/yuin/gopher-lua"
)

type World struct {
	l         *lua.LState
	fileStack []FileInfo
	types     map[string]Type
	formats   map[string]Format
	sources   map[string]Source
}

func NewWorld(l *lua.LState) *World {
	return &World{
		l:     l,
		types: map[string]Type{},
	}
}

// Library represents a Lua library.
type Library struct {
	// Name is the default name of the library.
	Name string
	// Open returns a table with the contents of the library.
	Open func(s State) *lua.LTable
}

// Open opens lib according to OpenAs by using the Name of the library.
func (w *World) Open(lib Library) error {
	return w.OpenAs(lib.Name, lib)
}

// OpenAs opens lib, then merges the result into the world's global table using
// name. If the name is already present and is a table, then the table of lib is
// merged into it, preferring the values of lib. If name is an empty string,
// then lib is merged into the global table itself. An error is returned if the
// merged value is not a table.
func (w *World) OpenAs(name string, lib Library) error {
	t := lib.Open(State{World: w, L: w.l})
	return w.mergeGlobal(name, t)
}

// mergeGlobal merges t into the global table according to name. Returns an
// error if t could not be merged.
func (w *World) mergeGlobal(name string, t *lua.LTable) error {
	if name == "" {
		t.ForEach(func(k, v lua.LValue) {
			if s, ok := k.(lua.LString); ok {
				w.l.SetGlobal(string(s), v)
			}
		})
		return nil
	}
	u := w.l.GetGlobal(name)
	switch u := u.(type) {
	case *lua.LTable:
		t.ForEach(func(k, v lua.LValue) { u.RawSet(k, v) })
	case *lua.LNilType:
		w.l.SetGlobal(name, t)
	default:
		return fmt.Errorf("cannot merge %s into %s", name, u.Type().String())
	}
	return nil
}

// Type returns the Type registered with the given name. If the name is not
// registered, then Type.Name will be an empty string.
func (w *World) Type(name string) Type {
	return w.types[name]
}

// createMetatable constructs a metatable from the given Type. If Members and
// Exprim is set, then the Value field will be injected if it does not already
// exist.
func (w *World) createMetatable(t Type) (mt *lua.LTable) {
	if t.Metatable == nil && t.Members == nil && t.Flags&Exprim == 0 {
		// No metatable.
		return nil
	}

	if t.Flags&Exprim != 0 {
		// Inject Value field, if possible.
		if t.Members == nil {
			t.Members = make(map[string]Member, 1)
		}
		if _, ok := t.Members["Value"]; !ok {
			t.Members["Value"] = Member{
				Get: func(s State, v types.Value) int {
					if v, ok := v.(types.Aliaser); ok {
						// Push underlying type.
						return s.Push(v.Alias())
					}
					// Fallback to current value.
					return s.Push(v)
				},
			}
		}
	}
	if t.Members != nil {
		// Validate members.
		for _, member := range t.Members {
			if member.Get == nil {
				panic("member must define Get function")
			}
		}
	}

	mt = w.l.CreateTable(0, 8)

	// Unconditional fields.
	mt.RawSetString("__type", lua.LString(t.Name))
	mt.RawSetString("__metatable", lua.LString("the metatable is locked"))

	if t.Flags&Exprim != 0 {
		// Show type and value, if possible.
		mt.RawSetString("__tostring", w.l.NewFunction(func(l *lua.LState) int {
			if u, ok := l.Get(1).(*lua.LUserData); ok {
				if v, ok := u.Value.(types.Stringer); ok {
					l.Push(lua.LString(t.Name + ": " + v.String()))
					return 1
				}
			}
			l.Push(lua.LString(t.Name))
			return 1
		}))
	} else {
		// Just show type.
		mt.RawSetString("__tostring", w.l.NewFunction(func(l *lua.LState) int {
			l.Push(lua.LString(t.Name))
			return 1
		}))
	}

	var index Metamethod
	var newindex Metamethod
	if t.Metatable != nil {
		// Set each defined metamethod, overriding predefined values.
		for name, method := range t.Metatable {
			m := method
			mt.RawSetString(name, w.WrapFunc(m))
		}
		// If available, remember index and newindex for member indexing.
		index = t.Metatable["__index"]
		newindex = t.Metatable["__newindex"]
	}

	if t.Members != nil {
		// Setup member getting and setting.
		mt.RawSetString("__index", w.l.NewFunction(func(l *lua.LState) int {
			u := l.CheckUserData(1)
			if u.Metatable != mt {
				TypeError(l, 1, t.Name)
				return 0
			}
			v, ok := u.Value.(types.Value)
			if !ok {
				TypeError(l, 1, t.Name)
				return 0
			}
			idx := l.Get(2)
			name, ok := idx.(lua.LString)
			if ok {
				member, ok := t.Members[string(name)]
				if !ok {
					goto customIndex
				}
				if member.Method {
					// Push as method.
					l.Push(l.NewFunction(func(l *lua.LState) int {
						// TODO: validate that s.L.Get(1) matches v, or at least has
						// the expected type.
						return member.Get(State{World: w, L: l}, v)
					}))
					return 1
				}
				return member.Get(State{World: w, L: l}, v)
			}
		customIndex:
			if index != nil {
				// Fallback to custom index.
				return index(State{World: w, L: l})
			}
			if ok {
				l.RaiseError("%q is not a valid member of %s", name, t.Name)
			} else {
				l.ArgError(2, "string expected, got "+idx.Type().String())
			}
			return 0
		}))
		mt.RawSetString("__newindex", w.l.NewFunction(func(l *lua.LState) int {
			u := l.CheckUserData(1)
			if u.Metatable != mt {
				TypeError(l, 1, t.Name)
				return 0
			}
			v, ok := u.Value.(types.Value)
			if !ok {
				TypeError(l, 1, t.Name)
				return 0
			}
			idx := l.Get(2)
			name, ok := idx.(lua.LString)
			if ok {
				member, ok := t.Members[string(name)]
				if !ok {
					goto customNewindex
				}
				if member.Method || member.Set == nil {
					l.RaiseError("%s cannot be assigned to", name)
				}
				member.Set(State{World: w, L: l}, v)
				return 0
			}
		customNewindex:
			if newindex != nil {
				// Fallback to custom newindex.
				return newindex(State{World: w, L: l})
			}
			if ok {
				l.RaiseError("%q is not a valid member of %s", name, t.Name)
			} else {
				l.ArgError(2, "string expected, got "+idx.Type().String())
			}
			return 0
		}))
	}

	return mt
}

// RegisterType registers a type. Panics if the type is already registered.
func (w *World) RegisterType(t Type) {
	if _, ok := w.types[t.Name]; ok {
		panic("type " + t.Name + " already registered")
	}
	if w.types == nil {
		w.types = map[string]Type{}
	}
	w.types[t.Name] = t

	if mt := w.createMetatable(t); mt != nil {
		w.l.SetField(w.l.Get(lua.RegistryIndex), t.Name, mt)
	}
	if t.Constructors != nil {
		ctors := w.l.CreateTable(0, len(t.Constructors))
		for name, ctor := range t.Constructors {
			c := ctor
			ctors.RawSetString(name, w.l.NewFunction(func(l *lua.LState) int {
				return c(State{World: w, L: w.l})
			}))
		}
		w.l.SetGlobal(t.Name, ctors)
	}
	if t.Environment != nil {
		t.Environment(State{World: w, L: w.l})
	}
}

// Types returns a list of types that have all of the given flags set.
func (w *World) Types(flags TypeFlags) []Type {
	ts := []Type{}
	for _, t := range w.types {
		if t.Flags&flags == flags {
			ts = append(ts, t)
		}
	}
	sort.Slice(ts, func(i, j int) bool {
		return ts[i].Name < ts[j].Name
	})
	return ts
}

// Format returns the Format registered with the given name. If the name is not
// registered, then Format.Name will be an empty string.
func (w *World) Format(name string) Format {
	return w.formats[strings.TrimPrefix(name, ".")]
}

// RegisterFormat registers a format. Panics if the format is already
// registered.
func (w *World) RegisterFormat(f Format) {
	f.Name = strings.TrimPrefix(f.Name, ".")
	if _, ok := w.formats[f.Name]; ok {
		panic("format " + f.Name + " already registered")
	}
	if w.formats == nil {
		w.formats = map[string]Format{}
	}
	w.formats[f.Name] = f
}

// Ext returns the extension of filename that most closely matches the name of a
// registered format. Returns an empty string if no format was found.
func (w *World) Ext(filename string) (ext string) {
	i := len(filename) - 1
	for ; i >= 0 && !os.IsPathSeparator(filename[i]); i-- {
	}
	filename = filename[i+1:]
	for {
		for i := 0; i < len(filename); i++ {
			if filename[i] == '.' {
				filename = filename[i+1:]
				goto check
			}
		}
		return ""
	check:
		if w.Format(filename).Name != "" {
			return filename
		}
	}
}

// Source returns the Source registered with the given name. If the name is not
// registered, then Source.Name will be an empty string.
func (w *World) Source(name string) Source {
	return w.sources[name]
}

// RegisterSource registers a source. Panics if the source is already
// registered, or the source's library could not be opened.
func (w *World) RegisterSource(s Source) {
	if _, ok := w.sources[s.Name]; ok {
		panic("source " + s.Name + " already registered")
	}
	if w.sources == nil {
		w.sources = map[string]Source{}
	}
	w.sources[s.Name] = s
	if s.Library.Open != nil {
		name := s.Library.Name
		if name == "" {
			name = s.Name
		}
		if err := w.OpenAs(name, s.Library); err != nil {
			panic(err.Error())
		}
	}
}

// State returns the underlying Lua state.
func (w *World) State() *lua.LState {
	return w.l
}

func (w *World) WrapFunc(f func(State) int) *lua.LFunction {
	return w.l.NewFunction(func(l *lua.LState) int {
		return f(State{World: w, L: l})
	})
}

// PushTo reflects v to lvs using registered type t.
func (w *World) PushTo(t string, v types.Value) (lvs []lua.LValue, err error) {
	typ := w.types[t]
	if typ.Name == "" {
		return nil, fmt.Errorf("unknown type %q", t)
	}
	if typ.PushTo == nil {
		return nil, fmt.Errorf("cannot cast type %q to Lua", t)
	}
	return typ.PushTo(State{World: w, L: w.l}, typ, v)
}

// PullFrom reflects lvs to v using registered type t.
func (w *World) PullFrom(t string, lvs ...lua.LValue) (v types.Value, err error) {
	typ := w.types[t]
	if typ.Name == "" {
		return nil, fmt.Errorf("unknown type %q", t)
	}
	if typ.PullFrom == nil {
		return nil, fmt.Errorf("cannot cast type %q from Lua", t)
	}
	return typ.PullFrom(State{World: w, L: w.l}, typ, lvs...)
}

// Push reflects v according to its type as registered, then pushes the results
// to the world's state.
func (w *World) Push(v types.Value) int {
	return State{World: w, L: w.l}.Push(v)
}

// Pull gets from the world's Lua state the values starting from n, and reflects
// a value from them according to registered type t.
func (w *World) Pull(n int, t string) types.Value {
	return State{World: w, L: w.l}.Pull(n, t)
}

// PullOpt gets from the world's Lua state the value at n, and reflects a value
// from it according to registered type t. If the value is nil, d is returned
// instead.
func (w *World) PullOpt(n int, t string, d types.Value) types.Value {
	return State{World: w, L: w.l}.PullOpt(n, t, d)
}

// PullAnyOf gets from the world's Lua state the values starting from n, and
// reflects a value from them according to any of the registered types in t.
// Returns the first successful reflection among the types in t. If no types
// succeeded, then a type error is thrown.
func (w *World) PullAnyOf(n int, t ...string) types.Value {
	return State{World: w, L: w.l}.PullAnyOf(n, t...)
}

type FileInfo struct {
	Path string
	os.FileInfo
}

// PushFile marks a file as the currently running file.
func (w *World) PushFile(fi FileInfo) error {
	for _, f := range w.fileStack {
		if os.SameFile(fi.FileInfo, f.FileInfo) {
			return fmt.Errorf("\"%s\" is already running", fi.Path)
		}
	}
	w.fileStack = append(w.fileStack, fi)
	return nil
}

// PopFile unmarks the currently running file.
func (w *World) PopFile() {
	if len(w.fileStack) > 0 {
		w.fileStack[len(w.fileStack)-1] = FileInfo{}
		w.fileStack = w.fileStack[:len(w.fileStack)-1]
	}
}

// PeekFile returns the info of the currently running file. Returns false if
// there is no running file.
func (w *World) PeekFile() (fi FileInfo, ok bool) {
	if len(w.fileStack) == 0 {
		return
	}
	fi = w.fileStack[len(w.fileStack)-1]
	ok = true
	return
}

// DoString executes string s as Lua. args is the number of arguments currently
// on the stack that should be passed in.
func (w *World) DoString(s, name string, args int) (err error) {
	fn, err := w.l.Load(strings.NewReader(s), name)
	if err != nil {
		return err
	}
	w.l.Insert(fn, -args-1)
	return w.l.PCall(args, lua.MultRet, nil)
}

// DoFile executes the contents of the file at fileName as Lua. args is the
// number of arguments currently on the stack that should be passed in. The file
// is marked as actively running, and is unmarked when the file returns.
func (w *World) DoFile(fileName string, args int) error {
	fi, err := os.Stat(fileName)
	if err != nil {
		return err
	}
	if err = w.PushFile(FileInfo{fileName, fi}); err != nil {
		return err
	}

	fn, err := w.l.LoadFile(fileName)
	if err != nil {
		w.PopFile()
		return err
	}
	w.l.Insert(fn, -args-1)
	err = w.l.PCall(args, lua.MultRet, nil)
	w.PopFile()
	return err
}

type File interface {
	Name() string
	Stat() (os.FileInfo, error)
	Read([]byte) (int, error)
	Write([]byte) (int, error)
}

// DoFile executes the contents of file f as Lua. args is the number of
// arguments currently on the stack that should be passed in. The file is marked
// as actively running, and is unmarked when the file returns.
func (w *World) DoFileHandle(f File, args int) error {
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if err = w.PushFile(FileInfo{f.Name(), fi}); err != nil {
		return err
	}

	fn, err := w.l.Load(f, fi.Name())
	if err != nil {
		w.PopFile()
		return err
	}
	w.l.Insert(fn, -args-1)
	err = w.l.PCall(args, lua.MultRet, nil)
	w.PopFile()
	return err
}
