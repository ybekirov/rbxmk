package reflect

import (
	. "github.com/anaminus/rbxmk"
	"github.com/yuin/gopher-lua"
)

func Token() Type {
	return Type{
		Name: "token",
		ReflectTo: func(s State, t Type, v Value) (lvs []lua.LValue, err error) {
			return []lua.LValue{lua.LNumber(v.(uint32))}, nil
		},
		ReflectFrom: func(s State, t Type, lvs ...lua.LValue) (v Value, err error) {
			if n, ok := lvs[0].(lua.LNumber); ok {
				return uint32(n), nil
			}
			return nil, TypeError(nil, 0, "token")
		},
	}
}