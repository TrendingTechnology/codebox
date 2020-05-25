package main

import (
	"errors"
	"flag"
	"fmt"
	"go/types"
	"sort"
	"strings"
	"sync"

	. "github.com/dave/jennifer/jen"
	"github.com/gagliardetto/codebox/scanner"
	. "github.com/gagliardetto/utils"
	"github.com/gin-gonic/gin"
	"github.com/iancoleman/strcase"
)

var (
	mu = &sync.RWMutex{}
)

type IndexItem struct {
	original interface{}
}

//
func NewIndexItem(v interface{}) *IndexItem {
	item := &IndexItem{}
	item.Set(v)
	return item
}

//
func (item *IndexItem) Set(v interface{}) {
	item.original = v
}

//
func (item *IndexItem) IsNil() bool {
	return item.original == nil
}

//
func (item *IndexItem) GetFEFunc() *FEFunc {
	fe, ok := item.original.(*FEFunc)
	if !ok {
		return nil
	}
	return fe
}

//
func (item *IndexItem) GetFETypeMethod() *FETypeMethod {
	fe, ok := item.original.(*FETypeMethod)
	if !ok {
		return nil
	}
	return fe
}

//
func (item *IndexItem) GetFEInterfaceMethod() *FEInterfaceMethod {
	fe, ok := item.original.(*FEInterfaceMethod)
	if !ok {
		return nil
	}
	return fe
}

type Index struct {
	mu     *sync.RWMutex
	values map[string]*IndexItem
}

func NewIndex() *Index {
	return &Index{
		mu:     &sync.RWMutex{},
		values: make(map[string]*IndexItem),
	}
}
func (index *Index) GetBySignature(signature string) *IndexItem {
	index.mu.RLock()
	defer index.mu.RUnlock()

	val, ok := index.values[signature]
	if !ok {
		return nil
	}
	return val
}

func (index *Index) Set(signature string, v interface{}) {
	index.mu.Lock()
	defer index.mu.Unlock()

	index.values[signature] = NewIndexItem(v)
}
func (index *Index) MustSetUnique(signature string, v interface{}) {

	existing := index.GetBySignature(signature)
	if existing != nil {
		panic(Sf("%q already in the index"))
	}

	index.Set(signature, v)
}

func main() {
	var pkg string
	var runServer bool
	flag.StringVar(&pkg, "pkg", "", "package you want to scan and convert to goa types")
	flag.BoolVar(&runServer, "http", false, "run http server")
	flag.Parse()

	// One package at a time:
	sc, err := scanner.New(pkg)
	if err != nil {
		panic(err)
	}

	pks, err := sc.Scan()
	if err != nil {
		panic(err)
	}

	feModule := &FEModule{
		Funcs:            make([]*FEFunc, 0),
		TypeMethods:      make([]*FETypeMethod, 0),
		InterfaceMethods: make([]*FEInterfaceMethod, 0),
	}

	pk := pks[0]
	{
		feModule.Name = pk.Name
		feModule.FEName = pk.Path
		feModule.PkgPath = scanner.RemoveGoSrcClonePath(pk.Path)
		feModule.PkgName = pk.Name

		for _, fn := range pk.Funcs {
			if fn.Receiver == nil {
				f := getFEFunc(fn)
				// TODO: what to do with aliases???
				f.PkgPath = feModule.PkgPath
				feModule.Funcs = append(feModule.Funcs, f)
			}
		}
		for _, mt := range pk.Methods {
			meth := getFETypeMethod(mt, pk.Funcs)
			if meth != nil {
				feModule.TypeMethods = append(feModule.TypeMethods, meth)
			}
		}
		for _, it := range pk.Interfaces {
			feModule.InterfaceMethods = append(feModule.InterfaceMethods, getAllFEInterfaceMethods(it)...)
		}
	}

	// Sort methods by receiver:
	sort.Slice(feModule.TypeMethods, func(i, j int) bool {
		// If same receiver...
		if feModule.TypeMethods[i].Receiver.QualifiedName == feModule.TypeMethods[j].Receiver.QualifiedName {
			// ... sort by func name:
			return feModule.TypeMethods[i].Func.Name < feModule.TypeMethods[j].Func.Name
		}
		return feModule.TypeMethods[i].Receiver.QualifiedName < feModule.TypeMethods[j].Receiver.QualifiedName
	})
	// Sort funcs by name:
	sort.Slice(feModule.Funcs, func(i, j int) bool {
		return feModule.Funcs[i].Name < feModule.Funcs[j].Name
	})

	//Q(feModule)
	Sfln(
		"package %q has %v funcs, %v methods, and %v interfaces",
		pk.Name,
		len(feModule.Funcs),
		len(feModule.TypeMethods),
		len(feModule.InterfaceMethods),
	)

	// Create index, and load values to it:
	index := NewIndex()
	{
		for _, v := range feModule.Funcs {
			index.MustSetUnique(v.Signature, v)
		}
		for _, v := range feModule.TypeMethods {
			index.MustSetUnique(v.Func.Signature, v)
		}
		for _, v := range feModule.InterfaceMethods {
			index.MustSetUnique(v.Func.Signature, v)
		}
	}
	if runServer {
		r := gin.Default()
		r.StaticFile("", "./index.html")
		r.Static("/static", "./static")

		r.GET("/api/source", func(c *gin.Context) {
			c.IndentedJSON(200, feModule)
		})
		r.POST("/api/pointers", func(c *gin.Context) {
			var req PayloadSetPointers
			err := c.BindJSON(&req)
			if err != nil {
				Errorf("error binding JSON: %s", err)
				return
			}
			Q(req)

			if err := req.Validate(); err != nil {
				Errorf("invalid request: %s", err)
				return
			}

			mu.Lock()
			defer mu.Unlock()

			stored := index.GetBySignature(req.Signature)
			if stored == nil {
				Errorf("not found: %q", req.Signature)
				c.Status(404)
				return
			}

			file := NewFile("main")
			{
				// main function:
				file.Func().Id("main").Params().Block()
			}
			{
				// sink function:
				code := Func().Id("sink").
					ParamsFunc(
						func(group *Group) {
							group.Add(Id("v").Interface())
						}).
					Block()
				file.Add(code.Line())
			}
			switch stored.original.(type) {
			case *FEFunc:
				{
					fe := stored.GetFEFunc()
					fe.CodeQL.Pointers = req.Pointers

					code := generate_ParaFuncPara(
						file,
						stored,
						MediumFunc,
						req.Pointers.Inp.Element,
						req.Pointers.Outp.Element,
					)
					if code != nil {
						// TODO: save `code` inside `fe` (add all to the file only at program exit).
						file.Add(code.Line())
					} else {
						Warnf("NOTHING GENERATED")
					}

				}
			case *FETypeMethod:
				{
					fe := stored.GetFETypeMethod()
					fe.CodeQL.Pointers = req.Pointers
				}
			case *FEInterfaceMethod:
				{
					fe := stored.GetFEInterfaceMethod()
					fe.CodeQL.Pointers = req.Pointers
				}
			default:
				panic(Sf("unknown type for %v", stored.original))
			}
			fmt.Printf("%#v", file)
			c.Status(200)
		})

		r.Run() // listen and serve on 0.0.0.0:8080
	}
}

// ShouldUseAlias tells whether the package name and the base
// of the backage path are the same; if they are not,
// then the package should use an alias in the import.
func ShouldUseAlias(pkgPath string, pkgName string) bool {
	lastSlashAt := strings.LastIndex(pkgPath, "/")
	if lastSlashAt == -1 {
		return pkgPath != pkgName
	}
	return pkgPath[lastSlashAt:] != pkgName
}

func generate_ParaFuncPara(file *File, item *IndexItem, medium Medium, fromElem Element, intoElem Element) *Statement {

	if medium == MediumFunc && fromElem == ElementParameter && intoElem == ElementParameter {
		{ //OK
			// from: param
			// medium: func
			// into: param
			fe := item.GetFEFunc()
			indexIn := fe.CodeQL.Pointers.Inp.Index
			indexOut := fe.CodeQL.Pointers.Outp.Index
			code := Func().Id("TaintStepTest_" + FormatCodeQlName(fe.Name)).
				ParamsFunc(
					func(group *Group) {
						group.Add(Id("sourceCQL").Interface())
					}).
				BlockFunc(
					func(group *Group) {
						group.BlockFunc(
							func(groupCase *Group) {
								inParam := fe.Parameters[indexIn]
								outParam := fe.Parameters[indexOut]
								// TODO: check if same index.

								inVarName := inParam.VarName
								outVarName := outParam.VarName
								groupCase.Comment(Sf("The flow is from `%s` into `%s`.", inVarName, outVarName)).Line()

								groupCase.Comment(Sf("Assume that `sourceCQL` has the underlying type of `%s`:", inVarName))
								composeTypeAssertion(file, groupCase, inParam.VarName, inParam.original.GetType())

								groupCase.Line().Comment(Sf("Declare `%s` variable:", outVarName))
								//groupCase.Var().Id(outParam.VarName).Qual(outParam.PkgPath, outParam.TypeName)
								composeVarDeclaration(file, groupCase, outParam.VarName, outParam.original.GetType())

								groupCase.
									Line().Comment("Call medium method that transfers the taint").
									Line().Comment(Sf("from the parameter `%s` to parameter `%s`;", inVarName, outVarName)).
									Line().Comment(Sf("`%s` is now tainted.", outVarName))

								importPackage(file, fe.PkgPath, fe.PkgName)

								groupCase.Qual(fe.PkgPath, fe.Name).CallFunc(
									func(call *Group) {

										tpFun := fe.original.GetType().(*types.Signature)

										zeroVals := scanTupleOfZeroValues(file, tpFun.Params())

										for i, zero := range zeroVals {
											isConsidered := i == indexIn || i == indexOut
											if isConsidered {
												call.Id(fe.Parameters[i].VarName)
											} else {
												call.Add(zero)
											}
										}

									},
								)

								groupCase.Line().Comment(Sf("Sink the tainted `%s`:", outVarName))
								groupCase.Id("sink").Call(Id(outParam.VarName))
							})
					})

			return code.Line()
		}
	}

	return nil
}
func scanTupleOfZeroValues(file *File, tuple *types.Tuple) []Code {

	result := make([]Code, 0)

	for i := 0; i < tuple.Len(); i++ {
		tp := newStatement()

		if tp != nil {
			composeZeroDeclaration(file, tp, tuple.At(i).Type())
			result = append(result, tp)
		}
	}

	return result
}
func composeZeroDeclaration(file *File, stat *Statement, typ types.Type) {
	switch t := typ.(type) {
	case *types.Basic:
		{
			switch t.Name() {
			case "bool":
				{
					stat.Lit(false)
				}
			case "string":
				{
					stat.Lit("")
				}
			case "int", "int8", "int16", "int32", "int64",
				"uint", "uint8", "uint16", "uint32", "uint64",
				"uintptr":
				{
					stat.Lit(0)
				}
			case "float32", "float64":
				{
					stat.Lit(0.0)
				}
			case "byte":
				{
					stat.Lit(0)
				}
			case "rune":
				{
					stat.Lit(0)
				}
			case "complex64", "complex128":
				{
					stat.Lit(0)
				}
			default:
				Errorf("unknown typeName: %q of kind %q", t.String(), t.Kind())
			}
		}
	case *types.Array:
		{
			stat.Nil()
		}
	case *types.Slice:
		{
			stat.Nil()
		}
	case *types.Struct:
		{
			fields := make([]Code, 0)
			for i := 0; i < t.NumFields(); i++ {
				field := t.Field(i)
				fldStm := newStatement()
				fldStm.Id(field.Name())

				importPackage(file, scanner.RemoveGoPath(field.Pkg()), field.Pkg().Name())

				composeZeroDeclaration(file, fldStm, field.Type())
				fields = append(fields, fldStm)
			}
			stat.Struct(fields...).Block()
		}
	case *types.Pointer:
		{
			stat.Nil()
		}
	case *types.Tuple:
		{
			// TODO
			stat.Nil()
		}
	case *types.Signature:
		{
			stat.Nil()
		}
	case *types.Interface:
		{
			stat.Nil()
		}
	case *types.Map:
		{
			stat.Nil()
		}
	case *types.Chan:
		{
			stat.Nil()
		}
	case *types.Named:
		{
			importPackage(file, scanner.RemoveGoPath(t.Obj().Pkg()), t.Obj().Pkg().Name())

			switch named := t.Underlying().(type) {
			case *types.Basic:
				{
					composeZeroDeclaration(file, stat, named)
				}
			case *types.Array:
				{
					composeZeroDeclaration(file, stat, named)
				}
			case *types.Slice:
				{
					composeZeroDeclaration(file, stat, named)
				}
			case *types.Struct:
				{
					stat.Qual(scanner.RemoveGoPath(t.Obj().Pkg()), t.Obj().Name()).Block()
				}
			case *types.Pointer:
				{
					composeZeroDeclaration(file, stat, named)
				}
			case *types.Tuple:
				{
					composeZeroDeclaration(file, stat, named)
				}
			case *types.Signature:
				{
					composeZeroDeclaration(file, stat, named)
				}
			case *types.Interface:
				{
					composeZeroDeclaration(file, stat, named)
				}
			case *types.Map:
				{
					composeZeroDeclaration(file, stat, named)
				}
			case *types.Chan:
				{
					composeZeroDeclaration(file, stat, named)
				}
			case *types.Named:
				{
					composeZeroDeclaration(file, stat, named)
				}

			}
		}
	}

}

// declare `name := sourceCQL.(Type)`
func composeTypeAssertion(file *File, group *Group, varName string, typ types.Type) {
	assertContent := newStatement()
	composeTypeDeclaration(file, assertContent, typ)
	group.Id(varName).Op(":=").Id("sourceCQL").Assert(assertContent)
}

// declare `var name Type`
func composeVarDeclaration(file *File, group *Group, varName string, typ types.Type) {
	composeTypeDeclaration(file, group.Var().Id(varName), typ)
}
func newStatement() *Statement {
	return &Statement{}
}

func importPackage(file *File, pkgPath string, pkgName string) {
	if ShouldUseAlias(pkgPath, pkgName) {
		file.ImportAlias(pkgPath, pkgName)
	} else {
		file.ImportName(pkgPath, pkgName)
	}
}

// composeTypeDeclaration adds the `Type` inside `var name Type`
func composeTypeDeclaration(file *File, stat *Statement, typ types.Type) {
	switch t := typ.(type) {
	case *types.Basic:
		{
			stat.Qual("", t.Name())
		}
	case *types.Array:
		{
			if t.Len() > 0 {
				stat.Index(Lit(t.Len()))
			} else {
				stat.Index()
			}
			composeTypeDeclaration(file, stat, t.Elem())
		}
	case *types.Slice:
		{
			stat.Index()
			composeTypeDeclaration(file, stat, t.Elem())
		}
	case *types.Struct:
		{
			fields := make([]Code, 0)
			for i := 0; i < t.NumFields(); i++ {
				field := t.Field(i)
				fldStm := newStatement()
				fldStm.Id(field.Name())

				importPackage(file, scanner.RemoveGoPath(field.Pkg()), field.Pkg().Name())

				composeTypeDeclaration(file, fldStm, field.Type())
				fields = append(fields, fldStm)
			}
			stat.Struct(fields...)
		}
	case *types.Pointer:
		{
			stat.Op("*")
			composeTypeDeclaration(file, stat, t.Elem())
		}
	case *types.Tuple:
		{
			// TODO
			tuple := scanTupleOfTypes(file, t, false)
			stat.Add(tuple...)
		}
	case *types.Signature:
		{
			paramsTuple := scanTupleOfTypes(file, t.Params(), t.Variadic())
			resultsTuple := scanTupleOfTypes(file, t.Results(), false)

			stat.Func().Params(paramsTuple...).List(resultsTuple...)
		}
	case *types.Interface:
		{
			if t.Empty() {
				stat.Interface()
			} else {
				// TODO
				methods := make([]Code, 0)
				for i := 0; i < t.NumMethods(); i++ {
					meth := t.Method(i)
					fn := newStatement()
					composeTypeDeclaration(file, fn, meth.Type())
					methods = append(methods, fn)
				}
				stat.Interface(methods...)
			}
		}
	case *types.Map:
		{
			mapKey := newStatement()
			composeTypeDeclaration(file, mapKey, t.Key())
			stat.Map(mapKey)
			composeTypeDeclaration(file, stat, t.Elem())
		}
	case *types.Chan:
		{

			switch t.Dir() {
			case types.SendRecv:
				stat.Chan()
			case types.RecvOnly:
				stat.Op("<-").Chan()
			case types.SendOnly:
				stat.Chan().Op("<-")
			}

			composeTypeDeclaration(file, stat, t.Elem())
		}
	case *types.Named:
		{
			importPackage(file, scanner.RemoveGoPath(t.Obj().Pkg()), t.Obj().Pkg().Name())
			stat.Qual(scanner.RemoveGoPath(t.Obj().Pkg()), t.Obj().Name())
		}
	}

}

func scanTupleOfTypes(file *File, tuple *types.Tuple, isVariadic bool) []Code {

	result := make([]Code, 0)

	for i := 0; i < tuple.Len(); i++ {
		tp := newStatement()

		if tp != nil {
			// If this is the last element,
			// and the function is variadic,
			// then set it to true:
			isLast := i == tuple.Len()-1
			if isLast && isVariadic {
				tp.Op("...")
			}
			composeTypeDeclaration(file, tp, tuple.At(i).Type())
			result = append(result, tp)
		}
	}

	return result
}

func setZeroOfParam(code *Group, param *FEType) {
	if param.IsNullable && !param.IsBasic {
		code.Nil()
		return
	}

	if param.IsStruct {
		code.Qual(param.PkgPath, param.TypeName).Block()
		return
	}

	if param.IsBasic {
		switch param.TypeName {
		case "bool":
			{
				code.Lit(false)
			}
		case "string":
			{
				code.Lit("")
			}
		case "int", "int8", "int16", "int32", "int64",
			"uint", "uint8", "uint16", "uint32", "uint64",
			"uintptr":
			{
				code.Lit(0)
			}
		case "float32", "float64":
			{
				code.Lit(0.0)
			}
		case "byte":
			{
				code.Lit(0)
			}
		case "rune":
			{
				code.Lit(0)
			}
		case "complex64", "complex128":
			{
				code.Lit(0)
			}
		default:
			Errorf("unknown typeName: %q from %q", param.TypeName, param.PkgPath)
		}
		return
	}

	Errorf("unknown typeName: %q from %q", param.TypeName, param.PkgPath)
}

type Medium string

const (
	MediumFunc   Medium = "function"
	MediumMethod Medium = "method" // either TypeMethod or InterfaceMethod
)

type PayloadSetPointers struct {
	Signature string
	Pointers  *CodeQLPointers
}

//
func (req *PayloadSetPointers) Validate() error {
	if req.Signature == "" {
		return errors.New("req.Signature is not set")
	}
	if req.Pointers == nil {
		return errors.New("req.Pointers is not set")
	}

	if err := req.Pointers.Validate(); err != nil {
		return err
	}

	return nil
}

func ToCamel(s string) string {
	return strcase.ToCamel(s)
}
func FormatCodeQlName(name string) string {
	return ToCamel(strings.ReplaceAll(name, "\"", ""))
}

const TODO = "TODO"

type CodeQLPointers struct {
	Inp  *Identity
	Outp *Identity
}

func (obj *CodeQLPointers) Validate() error {
	if obj.Inp == nil {
		return errors.New("obj.Inp is not set")
	}
	if obj.Outp == nil {
		return errors.New("obj.Outp is not set")
	}

	if err := obj.Inp.Validate(); err != nil {
		return err
	}
	if err := obj.Outp.Validate(); err != nil {
		return err
	}

	return nil
}
func (obj *Identity) Validate() error {
	if obj.Element == "" || obj.Element == TODO {
		return errors.New("obj.Element is not set")
	}

	// the Index can remain the default value only for the receiver:
	if obj.Index == -1 && obj.Element != ElementReceiver {
		return errors.New("obj.Index is not set")
	}
	return nil
}

var ValidElementNames = []string{
	string(ElementReceiver),
	string(ElementParameter),
	string(ElementResult),
}

func IsValidElementName(name Element) bool {
	return IsAnyOf(
		string(name),
		ValidElementNames...,
	)
}

func NewCodeQlFinalVals() *CodeQlFinalVals {
	return &CodeQlFinalVals{
		Inp:  TODO,
		Outp: TODO,
		Pointers: &CodeQLPointers{
			Inp: &Identity{
				Element: TODO,
				Index:   -1,
			},
			Outp: &Identity{
				Element: TODO,
				Index:   -1,
			},
		},
	}
}

type CodeQlFinalVals struct {
	Inp       string // string representation of the CodeQlIdentity.Placeholder
	Outp      string // string representation of the CodeQlIdentity.Placeholder
	IsEnabled bool
	Pointers  *CodeQLPointers // Pointers is where the current pointers will be stored
}

type Identity struct {
	Element Element
	Index   int
}
type CodeQlIdentity struct {
	Placeholder string
	Identity
}
type FEModule struct {
	Name             string
	PkgPath          string
	PkgName          string
	FEName           string
	Funcs            []*FEFunc
	TypeMethods      []*FETypeMethod
	InterfaceMethods []*FEInterfaceMethod
}

type FEFunc struct {
	CodeQL    *CodeQlFinalVals
	Signature string
	FEName    string
	Docs      []string
	Name      string
	PkgPath   string
	PkgName   string

	Parameters []*FEType
	Results    []*FEType
	original   *scanner.Func
}

func DocsWithDefault(docs []string) []string {
	if docs == nil {
		docs = make([]string, 0)
	}
	return docs
}

type Element string

const (
	ElementReceiver  Element = "receiver"
	ElementParameter Element = "parameter"
	ElementResult    Element = "result"
)

func getFEFunc(fn *scanner.Func) *FEFunc {
	var fe FEFunc
	fe.original = fn
	fe.CodeQL = NewCodeQlFinalVals()
	fe.Name = fn.Name
	fe.PkgName = fn.PkgName
	fe.FEName = FormatCodeQlName(fn.Name)
	fe.Docs = DocsWithDefault(fn.Doc)
	fe.Signature = RemoveThisPackagePathFromSignature(fn.Signature, fn.PkgPath)
	fe.PkgPath = fn.PkgPath
	for i, in := range fn.Input {
		v := getFEType(in)

		placeholder := Sf("isParameter(%v)", i)
		if v.IsVariadic {
			if len(fn.Input) == 1 {
				placeholder = "isParameter(_)"
			} else {
				placeholder = Sf("isParameter(any(int i | i >= %v))", i)
			}
		}
		isNotLast := i != len(fn.Input)-1
		if v.IsVariadic && isNotLast {
			panic(Sf("parameter %v is variadic but is NOT the last parameter", v))
		}
		v.Identity = CodeQlIdentity{
			Placeholder: placeholder,
			Identity: Identity{
				Element: ElementParameter,
				Index:   i,
			},
		}
		fe.Parameters = append(fe.Parameters, v)
	}
	for i, out := range fn.Output {
		v := getFEType(out)

		if len(fn.Output) == 1 {
			v.Identity = CodeQlIdentity{
				Placeholder: "isResult()",
				Identity: Identity{
					Element: ElementResult,
					Index:   i,
				},
			}
		} else {
			v.Identity = CodeQlIdentity{
				Placeholder: Sf("isResult(%v)", i),
				Identity: Identity{
					Element: ElementResult,
					Index:   i,
				},
			}
		}
		fe.Results = append(fe.Results, v)
	}
	return &fe
}
func RemoveThisPackagePathFromSignature(signature string, pkgPath string) string {
	clean := strings.Replace(signature, pkgPath+".", "", -1)
	return clean
}

type FEType struct {
	Identity      CodeQlIdentity
	VarName       string
	TypeName      string
	PkgName       string
	PkgPath       string
	QualifiedName string
	IsPtr         bool
	IsBasic       bool
	IsVariadic    bool
	IsNullable    bool
	IsStruct      bool
	IsRepeated    bool
	original      scanner.Type
}

func getFEType(tp scanner.Type) *FEType {
	var fe FEType
	fe.original = tp
	varName := tp.GetTypesVar().Name()
	if varName != "" {
		fe.VarName = varName
	}
	fe.IsVariadic = tp.IsVariadic()
	fe.IsNullable = tp.IsNullable()
	fe.IsPtr = tp.IsPtr()
	fe.IsStruct = tp.IsStruct()
	fe.IsBasic = tp.IsBasic()
	fe.IsRepeated = tp.IsRepeated()

	finalType := tp.GetTypesVar().Type()
	{
		slice, ok := tp.GetTypesVar().Type().(*types.Slice)
		if ok {
			finalType = slice.Elem()
		}
	}
	{
		array, ok := tp.GetTypesVar().Type().(*types.Array)
		if ok {
			finalType = array.Elem()
		}
	}
	// Check if pointer:
	{
		pointer, ok := finalType.(*types.Pointer)
		if ok {
			finalType = pointer.Elem()
		}
	}

	{
		named, ok := finalType.(*types.Named)
		if ok {
			fe.TypeName = named.Obj().Name()
			if pkg := named.Obj().Pkg(); pkg != nil {
				fe.QualifiedName = scanner.StringRemoveGoPath(pkg.Path()) + "." + named.Obj().Name()
				fe.PkgPath = scanner.RemoveGoPath(named.Obj().Pkg())
				fe.PkgName = named.Obj().Pkg().Name()
			}
		} else {
			fe.TypeName = tp.TypeString()
		}
	}

	return &fe
}

func getFETypeMethod(mt *types.Selection, allFuncs []*scanner.Func) *FETypeMethod {
	var fe FETypeMethod

	fe.CodeQL = NewCodeQlFinalVals()
	fe.Docs = make([]string, 0)

	fe.Receiver = &FEReceiver{}
	fe.Receiver.Identity = CodeQlIdentity{
		Placeholder: "isReceiver()",
		Identity: Identity{
			Element: ElementReceiver,
			Index:   -1,
		},
	}

	{
		var named *types.Named
		ptr, isPtr := mt.Recv().(*types.Pointer)
		if isPtr {
			named = ptr.Elem().(*types.Named)
		} else {
			named = mt.Recv().(*types.Named)
		}
		fe.Receiver.TypeName = named.Obj().Name()
		fe.Receiver.QualifiedName = scanner.RemoveGoPath(named.Obj().Pkg()) + "." + named.Obj().Name()
		fe.Receiver.PkgPath = scanner.RemoveGoPath(named.Obj().Pkg())
		//fe.Receiver.VarName =
	}

	fe.Func = &FEFunc{}
	methodFuncName := mt.Obj().Name()

	{
		// Check if the method is on a pointer of a value:
		_, isPtr := mt.Obj().Type().(*types.Signature).Recv().Type().(*types.Pointer)
		if isPtr {
			fe.IsOnPtr = true
		}
	}
	{
		findCorrespondingFunc := func() bool {
			for _, mtFn := range allFuncs {
				if mtFn.Receiver != nil {

					sameReceiverType := fe.Receiver.QualifiedName == mtFn.Receiver.TypeString()
					sameFuncName := methodFuncName == mtFn.Name

					if sameReceiverType && sameFuncName {
						fe.Docs = DocsWithDefault(mtFn.Doc)
						fe.Func = getFEFunc(mtFn)
						fe.original = mtFn.GetType()
						return true
					}
				}
			}
			return false
		}

		found := findCorrespondingFunc()
		if !found {
			return nil
		}
	}

	fe.FEName = fe.Receiver.TypeName + "-" + methodFuncName
	fe.ClassName = FormatCodeQlName(fe.Receiver.TypeName + "-" + methodFuncName)
	return &fe
}

type FETypeMethod struct {
	CodeQL    *CodeQlFinalVals
	ClassName string
	Docs      []string
	IsOnPtr   bool
	Receiver  *FEReceiver
	FEName    string
	Func      *FEFunc
	original  types.Type
}
type FEInterfaceMethod FETypeMethod

type FEReceiver FEType

func getFEInterfaceMethod(it *scanner.Interface, methodFunc *scanner.Func) *FETypeMethod {
	var fe FETypeMethod
	fe.original = it.GetType()

	fe.CodeQL = NewCodeQlFinalVals()

	fe.Receiver = &FEReceiver{}
	fe.Receiver.Identity = CodeQlIdentity{
		Placeholder: "isReceiver()",
		Identity: Identity{
			Element: ElementReceiver,
			Index:   -1,
		},
	}

	feFunc := getFEFunc(methodFunc)
	{

		fe.Receiver.TypeName = it.Name
		fe.Receiver.QualifiedName = scanner.StringRemoveGoPath(feFunc.PkgPath) + "." + feFunc.Name
		fe.Receiver.PkgPath = scanner.StringRemoveGoPath(feFunc.PkgPath)
	}

	fe.Func = &FEFunc{}
	methodFuncName := feFunc.Name

	{
		// Check if the method is on a pointer of a value:
		fe.IsOnPtr = true
	}
	{
		fe.Docs = DocsWithDefault(methodFunc.Doc)
		fe.Func = feFunc
	}

	fe.FEName = fe.Receiver.TypeName + "-" + methodFuncName
	fe.ClassName = FormatCodeQlName(fe.Receiver.TypeName + "-" + methodFuncName)
	return &fe
}
func getAllFEInterfaceMethods(it *scanner.Interface) []*FEInterfaceMethod {

	feInterfaces := make([]*FEInterfaceMethod, 0)
	for _, mt := range it.Methods {

		feMethod := getFEInterfaceMethod(it, mt)
		converted := FEInterfaceMethod(*feMethod)
		feInterfaces = append(feInterfaces, &converted)
	}
	return feInterfaces
}
