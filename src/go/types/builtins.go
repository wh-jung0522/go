// Copyright 2012 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This file implements typechecking of builtin function calls.

package types

import (
	"go/ast"
	"go/constant"
	"go/token"
)

// builtin type-checks a call to the built-in specified by id and
// reports whether the call is valid, with *x holding the result;
// but x.expr is not set. If the call is invalid, the result is
// false, and *x is undefined.
//
func (check *Checker) builtin(x *operand, call *ast.CallExpr, id builtinId) (_ bool) {
	// append is the only built-in that permits the use of ... for the last argument
	bin := predeclaredFuncs[id]
	if call.Ellipsis.IsValid() && id != _Append {
		check.invalidOp(atPos(call.Ellipsis),
			_InvalidDotDotDot,
			"invalid use of ... with built-in %s", bin.name)
		check.use(call.Args...)
		return
	}

	// For len(x) and cap(x) we need to know if x contains any function calls or
	// receive operations. Save/restore current setting and set hasCallOrRecv to
	// false for the evaluation of x so that we can check it afterwards.
	// Note: We must do this _before_ calling exprList because exprList evaluates
	//       all arguments.
	if id == _Len || id == _Cap {
		defer func(b bool) {
			check.hasCallOrRecv = b
		}(check.hasCallOrRecv)
		check.hasCallOrRecv = false
	}

	// determine actual arguments
	var arg func(*operand, int) // TODO(gri) remove use of arg getter in favor of using xlist directly
	nargs := len(call.Args)
	switch id {
	default:
		// make argument getter
		xlist, _ := check.exprList(call.Args, false)
		arg = func(x *operand, i int) { *x = *xlist[i] }
		nargs = len(xlist)
		// evaluate first argument, if present
		if nargs > 0 {
			arg(x, 0)
			if x.mode == invalid {
				return
			}
		}
	case _Make, _New, _Offsetof, _Trace:
		// arguments require special handling
	}

	// check argument count
	{
		msg := ""
		if nargs < bin.nargs {
			msg = "not enough"
		} else if !bin.variadic && nargs > bin.nargs {
			msg = "too many"
		}
		if msg != "" {
			check.invalidOp(inNode(call, call.Rparen), _WrongArgCount, "%s arguments for %s (expected %d, found %d)", msg, call, bin.nargs, nargs)
			return
		}
	}

	switch id {
	case _Append:
		// append(s S, x ...T) S, where T is the element type of S
		// spec: "The variadic function append appends zero or more values x to s of type
		// S, which must be a slice type, and returns the resulting slice, also of type S.
		// The values x are passed to a parameter of type ...T where T is the element type
		// of S and the respective parameter passing rules apply."
		S := x.typ
		var T Type
		if s := asSlice(S); s != nil {
			T = s.elem
		} else {
			check.invalidArg(x, _InvalidAppend, "%s is not a slice", x)
			return
		}

		// remember arguments that have been evaluated already
		alist := []operand{*x}

		// spec: "As a special case, append also accepts a first argument assignable
		// to type []byte with a second argument of string type followed by ... .
		// This form appends the bytes of the string.
		if nargs == 2 && call.Ellipsis.IsValid() {
			if ok, _ := x.assignableTo(check, NewSlice(universeByte), nil); ok {
				arg(x, 1)
				if x.mode == invalid {
					return
				}
				if isString(x.typ) {
					if check.Types != nil {
						sig := makeSig(S, S, x.typ)
						sig.variadic = true
						check.recordBuiltinType(call.Fun, sig)
					}
					x.mode = value
					x.typ = S
					break
				}
				alist = append(alist, *x)
				// fallthrough
			}
		}

		// check general case by creating custom signature
		sig := makeSig(S, S, NewSlice(T)) // []T required for variadic signature
		sig.variadic = true
		var xlist []*operand
		// convert []operand to []*operand
		for i := range alist {
			xlist = append(xlist, &alist[i])
		}
		for i := len(alist); i < nargs; i++ {
			var x operand
			arg(&x, i)
			xlist = append(xlist, &x)
		}
		check.arguments(call, sig, nil, xlist) // discard result (we know the result type)
		// ok to continue even if check.arguments reported errors

		x.mode = value
		x.typ = S
		if check.Types != nil {
			check.recordBuiltinType(call.Fun, sig)
		}

	case _Cap, _Len:
		// cap(x)
		// len(x)
		mode := invalid
		var typ Type
		var val constant.Value
		switch typ = arrayPtrDeref(under(x.typ)); t := typ.(type) {
		case *Basic:
			if isString(t) && id == _Len {
				if x.mode == constant_ {
					mode = constant_
					val = constant.MakeInt64(int64(len(constant.StringVal(x.val))))
				} else {
					mode = value
				}
			}

		case *Array:
			mode = value
			// spec: "The expressions len(s) and cap(s) are constants
			// if the type of s is an array or pointer to an array and
			// the expression s does not contain channel receives or
			// function calls; in this case s is not evaluated."
			if !check.hasCallOrRecv {
				mode = constant_
				if t.len >= 0 {
					val = constant.MakeInt64(t.len)
				} else {
					val = constant.MakeUnknown()
				}
			}

		case *Slice, *Chan:
			mode = value

		case *Map:
			if id == _Len {
				mode = value
			}

		case *TypeParam:
			if t.underIs(func(t Type) bool {
				switch t := arrayPtrDeref(t).(type) {
				case *Basic:
					if isString(t) && id == _Len {
						return true
					}
				case *Array, *Slice, *Chan:
					return true
				case *Map:
					if id == _Len {
						return true
					}
				}
				return false
			}) {
				mode = value
			}
		}

		if mode == invalid && typ != Typ[Invalid] {
			code := _InvalidCap
			if id == _Len {
				code = _InvalidLen
			}
			check.invalidArg(x, code, "%s for %s", x, bin.name)
			return
		}

		x.mode = mode
		x.typ = Typ[Int]
		x.val = val
		if check.Types != nil && mode != constant_ {
			check.recordBuiltinType(call.Fun, makeSig(x.typ, typ))
		}

	case _Close:
		// close(c)
		if !underIs(x.typ, func(u Type) bool {
			uch, _ := u.(*Chan)
			if uch == nil {
				check.invalidOp(x, _InvalidClose, "cannot close non-channel %s", x)
				return false
			}
			if uch.dir == RecvOnly {
				check.invalidOp(x, _InvalidClose, "cannot close receive-only channel %s", x)
				return false
			}
			return true
		}) {
			return
		}
		x.mode = novalue
		if check.Types != nil {
			check.recordBuiltinType(call.Fun, makeSig(nil, x.typ))
		}

	case _Complex:
		// complex(x, y floatT) complexT
		var y operand
		arg(&y, 1)
		if y.mode == invalid {
			return
		}

		// convert or check untyped arguments
		d := 0
		if isUntyped(x.typ) {
			d |= 1
		}
		if isUntyped(y.typ) {
			d |= 2
		}
		switch d {
		case 0:
			// x and y are typed => nothing to do
		case 1:
			// only x is untyped => convert to type of y
			check.convertUntyped(x, y.typ)
		case 2:
			// only y is untyped => convert to type of x
			check.convertUntyped(&y, x.typ)
		case 3:
			// x and y are untyped =>
			// 1) if both are constants, convert them to untyped
			//    floating-point numbers if possible,
			// 2) if one of them is not constant (possible because
			//    it contains a shift that is yet untyped), convert
			//    both of them to float64 since they must have the
			//    same type to succeed (this will result in an error
			//    because shifts of floats are not permitted)
			if x.mode == constant_ && y.mode == constant_ {
				toFloat := func(x *operand) {
					if isNumeric(x.typ) && constant.Sign(constant.Imag(x.val)) == 0 {
						x.typ = Typ[UntypedFloat]
					}
				}
				toFloat(x)
				toFloat(&y)
			} else {
				check.convertUntyped(x, Typ[Float64])
				check.convertUntyped(&y, Typ[Float64])
				// x and y should be invalid now, but be conservative
				// and check below
			}
		}
		if x.mode == invalid || y.mode == invalid {
			return
		}

		// both argument types must be identical
		if !Identical(x.typ, y.typ) {
			check.invalidArg(x, _InvalidComplex, "mismatched types %s and %s", x.typ, y.typ)
			return
		}

		// the argument types must be of floating-point type
		f := func(x Type) Type {
			if t := asBasic(x); t != nil {
				switch t.kind {
				case Float32:
					return Typ[Complex64]
				case Float64:
					return Typ[Complex128]
				case UntypedFloat:
					return Typ[UntypedComplex]
				}
			}
			return nil
		}
		resTyp := check.applyTypeFunc(f, x.typ)
		if resTyp == nil {
			check.invalidArg(x, _InvalidComplex, "arguments have type %s, expected floating-point", x.typ)
			return
		}

		// if both arguments are constants, the result is a constant
		if x.mode == constant_ && y.mode == constant_ {
			x.val = constant.BinaryOp(constant.ToFloat(x.val), token.ADD, constant.MakeImag(constant.ToFloat(y.val)))
		} else {
			x.mode = value
		}

		if check.Types != nil && x.mode != constant_ {
			check.recordBuiltinType(call.Fun, makeSig(resTyp, x.typ, x.typ))
		}

		x.typ = resTyp

	case _Copy:
		// copy(x, y []T) int
		dst, _ := singleUnder(x.typ).(*Slice)

		var y operand
		arg(&y, 1)
		if y.mode == invalid {
			return
		}
		src, _ := singleUnderString(y.typ).(*Slice)

		if dst == nil || src == nil {
			check.invalidArg(x, _InvalidCopy, "copy expects slice arguments; found %s and %s", x, &y)
			return
		}

		if !Identical(dst.elem, src.elem) {
			check.errorf(x, _InvalidCopy, "arguments to copy %s and %s have different element types %s and %s", x, &y, dst.elem, src.elem)
			return
		}

		if check.Types != nil {
			check.recordBuiltinType(call.Fun, makeSig(Typ[Int], x.typ, y.typ))
		}
		x.mode = value
		x.typ = Typ[Int]

	case _Delete:
		// delete(map_, key)
		// map_ must be a map type or a type parameter describing map types.
		// The key cannot be a type parameter for now.
		map_ := x.typ
		var key Type
		if !underIs(map_, func(u Type) bool {
			map_, _ := u.(*Map)
			if map_ == nil {
				check.invalidArg(x, _InvalidDelete, "%s is not a map", x)
				return false
			}
			if key != nil && !Identical(map_.key, key) {
				check.invalidArg(x, _Todo, "maps of %s must have identical key types", x)
				return false
			}
			key = map_.key
			return true
		}) {
			return
		}

		arg(x, 1) // k
		if x.mode == invalid {
			return
		}

		check.assignment(x, key, "argument to delete")
		if x.mode == invalid {
			return
		}

		x.mode = novalue
		if check.Types != nil {
			check.recordBuiltinType(call.Fun, makeSig(nil, map_, key))
		}

	case _Imag, _Real:
		// imag(complexT) floatT
		// real(complexT) floatT

		// convert or check untyped argument
		if isUntyped(x.typ) {
			if x.mode == constant_ {
				// an untyped constant number can always be considered
				// as a complex constant
				if isNumeric(x.typ) {
					x.typ = Typ[UntypedComplex]
				}
			} else {
				// an untyped non-constant argument may appear if
				// it contains a (yet untyped non-constant) shift
				// expression: convert it to complex128 which will
				// result in an error (shift of complex value)
				check.convertUntyped(x, Typ[Complex128])
				// x should be invalid now, but be conservative and check
				if x.mode == invalid {
					return
				}
			}
		}

		// the argument must be of complex type
		f := func(x Type) Type {
			if t := asBasic(x); t != nil {
				switch t.kind {
				case Complex64:
					return Typ[Float32]
				case Complex128:
					return Typ[Float64]
				case UntypedComplex:
					return Typ[UntypedFloat]
				}
			}
			return nil
		}
		resTyp := check.applyTypeFunc(f, x.typ)
		if resTyp == nil {
			code := _InvalidImag
			if id == _Real {
				code = _InvalidReal
			}
			check.invalidArg(x, code, "argument has type %s, expected complex type", x.typ)
			return
		}

		// if the argument is a constant, the result is a constant
		if x.mode == constant_ {
			if id == _Real {
				x.val = constant.Real(x.val)
			} else {
				x.val = constant.Imag(x.val)
			}
		} else {
			x.mode = value
		}

		if check.Types != nil && x.mode != constant_ {
			check.recordBuiltinType(call.Fun, makeSig(resTyp, x.typ))
		}

		x.typ = resTyp

	case _Make:
		// make(T, n)
		// make(T, n, m)
		// (no argument evaluated yet)
		arg0 := call.Args[0]
		T := check.varType(arg0)
		if T == Typ[Invalid] {
			return
		}

		var min int // minimum number of arguments
		switch singleUnder(T).(type) {
		case *Slice:
			min = 2
		case *Map, *Chan:
			min = 1
		case nil:
			check.errorf(arg0, _InvalidMake, "cannot make %s; type set has no single underlying type", arg0)
			return
		default:
			check.invalidArg(arg0, _InvalidMake, "cannot make %s; type must be slice, map, or channel", arg0)
			return
		}
		if nargs < min || min+1 < nargs {
			check.invalidOp(call, _WrongArgCount, "%v expects %d or %d arguments; found %d", call, min, min+1, nargs)
			return
		}

		types := []Type{T}
		var sizes []int64 // constant integer arguments, if any
		for _, arg := range call.Args[1:] {
			typ, size := check.index(arg, -1) // ok to continue with typ == Typ[Invalid]
			types = append(types, typ)
			if size >= 0 {
				sizes = append(sizes, size)
			}
		}
		if len(sizes) == 2 && sizes[0] > sizes[1] {
			check.invalidArg(call.Args[1], _SwappedMakeArgs, "length and capacity swapped")
			// safe to continue
		}
		x.mode = value
		x.typ = T
		if check.Types != nil {
			check.recordBuiltinType(call.Fun, makeSig(x.typ, types...))
		}

	case _New:
		// new(T)
		// (no argument evaluated yet)
		T := check.varType(call.Args[0])
		if T == Typ[Invalid] {
			return
		}

		x.mode = value
		x.typ = &Pointer{base: T}
		if check.Types != nil {
			check.recordBuiltinType(call.Fun, makeSig(x.typ, T))
		}

	case _Panic:
		// panic(x)
		// record panic call if inside a function with result parameters
		// (for use in Checker.isTerminating)
		if check.sig != nil && check.sig.results.Len() > 0 {
			// function has result parameters
			p := check.isPanic
			if p == nil {
				// allocate lazily
				p = make(map[*ast.CallExpr]bool)
				check.isPanic = p
			}
			p[call] = true
		}

		check.assignment(x, &emptyInterface, "argument to panic")
		if x.mode == invalid {
			return
		}

		x.mode = novalue
		if check.Types != nil {
			check.recordBuiltinType(call.Fun, makeSig(nil, &emptyInterface))
		}

	case _Print, _Println:
		// print(x, y, ...)
		// println(x, y, ...)
		var params []Type
		if nargs > 0 {
			params = make([]Type, nargs)
			for i := 0; i < nargs; i++ {
				if i > 0 {
					arg(x, i) // first argument already evaluated
				}
				check.assignment(x, nil, "argument to "+predeclaredFuncs[id].name)
				if x.mode == invalid {
					// TODO(gri) "use" all arguments?
					return
				}
				params[i] = x.typ
			}
		}

		x.mode = novalue
		if check.Types != nil {
			check.recordBuiltinType(call.Fun, makeSig(nil, params...))
		}

	case _Recover:
		// recover() interface{}
		x.mode = value
		x.typ = &emptyInterface
		if check.Types != nil {
			check.recordBuiltinType(call.Fun, makeSig(x.typ))
		}

	case _Add:
		// unsafe.Add(ptr unsafe.Pointer, len IntegerType) unsafe.Pointer
		if !check.allowVersion(check.pkg, 1, 17) {
			check.errorf(call.Fun, _InvalidUnsafeAdd, "unsafe.Add requires go1.17 or later")
			return
		}

		check.assignment(x, Typ[UnsafePointer], "argument to unsafe.Add")
		if x.mode == invalid {
			return
		}

		var y operand
		arg(&y, 1)
		if !check.isValidIndex(&y, _InvalidUnsafeAdd, "length", true) {
			return
		}

		x.mode = value
		x.typ = Typ[UnsafePointer]
		if check.Types != nil {
			check.recordBuiltinType(call.Fun, makeSig(x.typ, x.typ, y.typ))
		}

	case _Alignof:
		// unsafe.Alignof(x T) uintptr
		check.assignment(x, nil, "argument to unsafe.Alignof")
		if x.mode == invalid {
			return
		}

		if hasVarSize(x.typ) {
			x.mode = value
			if check.Types != nil {
				check.recordBuiltinType(call.Fun, makeSig(Typ[Uintptr], x.typ))
			}
		} else {
			x.mode = constant_
			x.val = constant.MakeInt64(check.conf.alignof(x.typ))
			// result is constant - no need to record signature
		}
		x.typ = Typ[Uintptr]

	case _Offsetof:
		// unsafe.Offsetof(x T) uintptr, where x must be a selector
		// (no argument evaluated yet)
		arg0 := call.Args[0]
		selx, _ := unparen(arg0).(*ast.SelectorExpr)
		if selx == nil {
			check.invalidArg(arg0, _BadOffsetofSyntax, "%s is not a selector expression", arg0)
			check.use(arg0)
			return
		}

		check.expr(x, selx.X)
		if x.mode == invalid {
			return
		}

		base := derefStructPtr(x.typ)
		sel := selx.Sel.Name
		obj, index, indirect := LookupFieldOrMethod(base, false, check.pkg, sel)
		switch obj.(type) {
		case nil:
			check.invalidArg(x, _MissingFieldOrMethod, "%s has no single field %s", base, sel)
			return
		case *Func:
			// TODO(gri) Using derefStructPtr may result in methods being found
			// that don't actually exist. An error either way, but the error
			// message is confusing. See: https://play.golang.org/p/al75v23kUy ,
			// but go/types reports: "invalid argument: x.m is a method value".
			check.invalidArg(arg0, _InvalidOffsetof, "%s is a method value", arg0)
			return
		}
		if indirect {
			check.invalidArg(x, _InvalidOffsetof, "field %s is embedded via a pointer in %s", sel, base)
			return
		}

		// TODO(gri) Should we pass x.typ instead of base (and have indirect report if derefStructPtr indirected)?
		check.recordSelection(selx, FieldVal, base, obj, index, false)

		// record the selector expression (was bug - issue #47895)
		{
			mode := value
			if x.mode == variable || indirect {
				mode = variable
			}
			check.record(&operand{mode, selx, obj.Type(), nil, 0})
		}

		// The field offset is considered a variable even if the field is declared before
		// the part of the struct which is variable-sized. This makes both the rules
		// simpler and also permits (or at least doesn't prevent) a compiler from re-
		// arranging struct fields if it wanted to.
		if hasVarSize(base) {
			x.mode = value
			if check.Types != nil {
				check.recordBuiltinType(call.Fun, makeSig(Typ[Uintptr], obj.Type()))
			}
		} else {
			x.mode = constant_
			x.val = constant.MakeInt64(check.conf.offsetof(base, index))
			// result is constant - no need to record signature
		}
		x.typ = Typ[Uintptr]

	case _Sizeof:
		// unsafe.Sizeof(x T) uintptr
		check.assignment(x, nil, "argument to unsafe.Sizeof")
		if x.mode == invalid {
			return
		}

		if hasVarSize(x.typ) {
			x.mode = value
			if check.Types != nil {
				check.recordBuiltinType(call.Fun, makeSig(Typ[Uintptr], x.typ))
			}
		} else {
			x.mode = constant_
			x.val = constant.MakeInt64(check.conf.sizeof(x.typ))
			// result is constant - no need to record signature
		}
		x.typ = Typ[Uintptr]

	case _Slice:
		// unsafe.Slice(ptr *T, len IntegerType) []T
		if !check.allowVersion(check.pkg, 1, 17) {
			check.errorf(call.Fun, _InvalidUnsafeSlice, "unsafe.Slice requires go1.17 or later")
			return
		}

		typ := asPointer(x.typ)
		if typ == nil {
			check.invalidArg(x, _InvalidUnsafeSlice, "%s is not a pointer", x)
			return
		}

		var y operand
		arg(&y, 1)
		if !check.isValidIndex(&y, _InvalidUnsafeSlice, "length", false) {
			return
		}

		x.mode = value
		x.typ = NewSlice(typ.base)
		if check.Types != nil {
			check.recordBuiltinType(call.Fun, makeSig(x.typ, typ, y.typ))
		}

	case _Assert:
		// assert(pred) causes a typechecker error if pred is false.
		// The result of assert is the value of pred if there is no error.
		// Note: assert is only available in self-test mode.
		if x.mode != constant_ || !isBoolean(x.typ) {
			check.invalidArg(x, _Test, "%s is not a boolean constant", x)
			return
		}
		if x.val.Kind() != constant.Bool {
			check.errorf(x, _Test, "internal error: value of %s should be a boolean constant", x)
			return
		}
		if !constant.BoolVal(x.val) {
			check.errorf(call, _Test, "%v failed", call)
			// compile-time assertion failure - safe to continue
		}
		// result is constant - no need to record signature

	case _Trace:
		// trace(x, y, z, ...) dumps the positions, expressions, and
		// values of its arguments. The result of trace is the value
		// of the first argument.
		// Note: trace is only available in self-test mode.
		// (no argument evaluated yet)
		if nargs == 0 {
			check.dump("%v: trace() without arguments", call.Pos())
			x.mode = novalue
			break
		}
		var t operand
		x1 := x
		for _, arg := range call.Args {
			check.rawExpr(x1, arg, nil, false) // permit trace for types, e.g.: new(trace(T))
			check.dump("%v: %s", x1.Pos(), x1)
			x1 = &t // use incoming x only for first argument
		}
		// trace is only available in test mode - no need to record signature

	default:
		unreachable()
	}

	return true
}

// If typ is a type parameter, single under returns the single underlying
// type of all types in the corresponding type constraint if it exists, or
// nil if it doesn't exist. If typ is not a type parameter, singleUnder
// just returns the underlying type.
func singleUnder(typ Type) Type {
	var su Type
	if underIs(typ, func(u Type) bool {
		if su != nil && !Identical(su, u) {
			return false
		}
		// su == nil || Identical(su, u)
		su = u
		return true
	}) {
		return su
	}
	return nil
}

// singleUnderString is like singleUnder but also considers []byte and
// string as "identical". In this case, if successful, the result is always
// []byte.
func singleUnderString(typ Type) Type {
	var su Type
	if underIs(typ, func(u Type) bool {
		if isString(u) {
			u = NewSlice(universeByte)
		}
		if su != nil && !Identical(su, u) {
			return false
		}
		// su == nil || Identical(su, u)
		su = u
		return true
	}) {
		return su
	}
	return nil
}

// hasVarSize reports if the size of type t is variable due to type parameters.
func hasVarSize(t Type) bool {
	switch t := under(t).(type) {
	case *Array:
		return hasVarSize(t.elem)
	case *Struct:
		for _, f := range t.fields {
			if hasVarSize(f.typ) {
				return true
			}
		}
	case *TypeParam:
		return true
	case *Named, *Union, *top:
		unreachable()
	}
	return false
}

// applyTypeFunc applies f to x. If x is a type parameter,
// the result is a type parameter constrained by an new
// interface bound. The type bounds for that interface
// are computed by applying f to each of the type bounds
// of x. If any of these applications of f return nil,
// applyTypeFunc returns nil.
// If x is not a type parameter, the result is f(x).
func (check *Checker) applyTypeFunc(f func(Type) Type, x Type) Type {
	if tp := asTypeParam(x); tp != nil {
		// Test if t satisfies the requirements for the argument
		// type and collect possible result types at the same time.
		var terms []*Term
		if !tp.iface().typeSet().is(func(t *term) bool {
			if r := f(t.typ); r != nil {
				terms = append(terms, NewTerm(t.tilde, r))
				return true
			}
			return false
		}) {
			return nil
		}

		// Construct a suitable new type parameter for the sum type. The
		// type param is placed in the current package so export/import
		// works as expected.
		tpar := NewTypeName(token.NoPos, check.pkg, "<type parameter>", nil)
		ptyp := check.newTypeParam(tpar, NewInterfaceType(nil, []Type{NewUnion(terms)})) // assigns type to tpar as a side-effect
		ptyp.index = tp.index

		return ptyp
	}

	return f(x)
}

// makeSig makes a signature for the given argument and result types.
// Default types are used for untyped arguments, and res may be nil.
func makeSig(res Type, args ...Type) *Signature {
	list := make([]*Var, len(args))
	for i, param := range args {
		list[i] = NewVar(token.NoPos, nil, "", Default(param))
	}
	params := NewTuple(list...)
	var result *Tuple
	if res != nil {
		assert(!isUntyped(res))
		result = NewTuple(NewVar(token.NoPos, nil, "", res))
	}
	return &Signature{params: params, results: result}
}

// arrayPtrDeref returns A if typ is of the form *A and A is an array;
// otherwise it returns typ.
func arrayPtrDeref(typ Type) Type {
	if p, ok := typ.(*Pointer); ok {
		if a := asArray(p.base); a != nil {
			return a
		}
	}
	return typ
}

// unparen returns e with any enclosing parentheses stripped.
func unparen(e ast.Expr) ast.Expr {
	for {
		p, ok := e.(*ast.ParenExpr)
		if !ok {
			return e
		}
		e = p.X
	}
}
