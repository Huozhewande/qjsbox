/* Trusted bootstrap; embedded into the isolated worker, never evaluated in Go. */
(function () {
  'use strict';
  const parse = acorn.parse, NativeFunction = Function, NativeError = Error, hostRead = __hostRead;
  const own = Function.call.bind(Object.prototype.hasOwnProperty);
  const stringify = JSON.stringify, isFiniteNumber = Number.isFinite;
  // Capture every intrinsic used across the untrusted execution boundary. Global
  // bindings are also locked below: freezing a value alone does not lock its name.
  const reflectGet = Reflect.get, arrayIsArray = Array.isArray;
  const numberFrom = Number, maxNumber = Math.max;
  const freeze = Object.freeze, arrayPrototype = Array.prototype;
  const regexpExec = Function.call.bind(RegExp.prototype.exec);
  function fail(code, message, node) {
    const e = new NativeError(message); e.code = code;
    if (node && node.loc) { e.line = node.loc.start.line; e.column = node.loc.start.column + 1; }
    throw e;
  }
  function analyze(kind, code) {
    let ast;
    try { ast = parse(code, {ecmaVersion:2022, allowReturnOutsideFunction:true, locations:true}); }
    catch(e) { fail('SCRIPT_SYNTAX', e.message, e.loc ? {loc:{start:e.loc}} : null); }
    const refs = new Set(), helperTests = []; let helper = false;
    const stack = [[ast,null,null]];
    while(stack.length) {
      const [n,p,key] = stack.pop();
      if (['ImportExpression','ImportDeclaration','ExportNamedDeclaration','WithStatement','AwaitExpression','YieldExpression'].includes(n.type) || n.async || n.generator) fail('SCRIPT_FORBIDDEN','asynchronous code and modules are unavailable',n);
      // The public API is named input/context/factors only. The Function wrapper's
      // implicit arguments object would otherwise expose unanalysable aliases.
      if(n.type==='Identifier' && n.name==='arguments') fail('SCRIPT_REFERENCE','implicit arguments access is unavailable',n);
      if(n.type==='Identifier' && n.name==='factors') {
        if(kind!=='RULE' || !p || p.type!=='MemberExpression' || key!=='object' || p.optional) fail('SCRIPT_REFERENCE','factors must be accessed directly',n);
        const ref = p.computed ? p.property.type==='Literal' && typeof p.property.value==='string' ? p.property.value : null : p.property.name;
        if(!ref || !/^[A-Za-z_][A-Za-z0-9_]*$/.test(ref)) fail('SCRIPT_REFERENCE','factor reference must be a static identifier',p);
        refs.add(ref);
      }
      if(n.type==='Identifier' && n.name==='__rule') helper=true;
      for(const k of Object.keys(n)) {
        if(k==='loc') continue;
        const v=n[k];
        if(Array.isArray(v)){ for(const c of v) if(c && typeof c.type==='string')stack.push([c,n,k]); }
        else if(v && typeof v.type==='string')stack.push([v,n,k]);
      }
    }
    if(helper) {
      if(kind!=='RULE' || ast.body.length!==1 || ast.body[0].type!=='ReturnStatement') fail('SCRIPT_REFERENCE','generated helper code must be a single return expression');
      const literal=n=>{if(!n)return undefined;if(n.type==='Literal')return n.value;if(n.type==='UnaryExpression' && n.operator==='-' && n.argument.type==='Literal' && typeof n.argument.value==='number')return -n.argument.value;if(n.type==='ArrayExpression')return n.elements.map(literal);fail('SCRIPT_REFERENCE','helper operands must be literal',n)};
      function check(n) {
        if(n.type!=='CallExpression' || n.callee.type!=='MemberExpression' || n.callee.computed || n.callee.object.name!=='__rule')fail('SCRIPT_REFERENCE','invalid generated helper expression',n);
        const method=n.callee.property.name;
        if(method==='all'||method==='any') {
          if(n.arguments.length!==1 || n.arguments[0].type!=='ArrayExpression' || !n.arguments[0].elements.length)fail('SCRIPT_REFERENCE','helper group requires delayed children',n);
          for(const child of n.arguments[0].elements){if(!child || child.type!=='ArrowFunctionExpression' || child.params.length || child.body.type==='BlockStatement')fail('SCRIPT_REFERENCE','helper child must be a delayed expression',child);check(child.body);}
        } else if(method==='test') {
          const args=n.arguments.map(literal), ref=args[0],op=args[1];
          if(typeof ref!=='string' || !/^[^\s]+$/.test(ref) || !['EQUALS','GTE','LTE','IN','ANY_MATCH','EXISTS'].includes(op) || args.length<2 || args.length>3)fail('SCRIPT_REFERENCE','invalid helper test',n);
          const operand=args[2];
          const scalar=typeof operand==='string'||typeof operand==='boolean'||typeof operand==='number'&&isFiniteNumber(operand);
          const strings=arrayIsArray(operand)&&operand.length>0&&operand.every(x=>typeof x==='string'&&x.trim()!=='');
          const valid=op==='EXISTS'?args.length===2:args.length===3&&(op==='EQUALS'?scalar:op==='GTE'||op==='LTE'?typeof operand==='number'&&isFiniteNumber(operand):strings);
          if(!valid)fail('SCRIPT_REFERENCE','invalid operand for '+op,n);
          refs.add(ref);
          helperTests.push({reference:ref,operator:op,operand});
        } else fail('SCRIPT_REFERENCE','unknown helper',n);
      }
      check(ast.body[0].argument);
    }
    return {references:Array.from(refs).sort(),helper,helperTests};
  }
  globalThis.__run = function(req) {
    const analysis=analyze(req.kind,req.code);
    if(req.analyze)return {references:analysis.references,helperTests:analysis.helperTests};
    const allowed=new Set(req.allowedReferences||[]);
    for(const ref of analysis.references)if(!allowed.has(ref))fail('SCRIPT_REFERENCE','undeclared factor: '+ref);
    const compiled=NativeFunction('input','context','factors','__rule','"use strict";\n'+req.code);
    let missing=false;
    function readonly(v,depth) {
      if(depth>64)fail('SCRIPT_LIMIT','input depth exceeds limit');
      if(v===null || typeof v!=='object')return v;
      for(const k of Object.keys(v)) v[k]=readonly(v[k],depth+1);
      freeze(v);
      return new Proxy(v,{get(target,key,receiver){if(!own(target,key) && !(arrayIsArray(target)&&own(arrayPrototype,key))){missing=true;fail('SCRIPT_INPUT_MISSING','input property unavailable: '+(typeof key==='string'?key:'symbol'));}return reflectGet(target,key,receiver);},set(){fail('SCRIPT_INPUT','input is read only');},deleteProperty(){fail('SCRIPT_INPUT','input is read only');}});
    }
    const input=readonly(req.input,0), context=Object.freeze({now:req.now});
    const unavailable=Object.freeze(Object.create(null));
    const availableOptions=new Map();
    function read(ref,soft) {
      if(!allowed.has(ref))fail('SCRIPT_REFERENCE','undeclared factor');
      const v=hostRead(ref);
      let ok=!v.error;
      if(v.type==='TEXT')ok=ok && typeof v.value==='string';
      else if(v.type==='NUMBER')ok=ok && typeof v.value==='number' && isFiniteNumber(v.value);
      else if(v.type==='BOOLEAN')ok=ok && typeof v.value==='boolean';
      else if(soft && v.type==='SET')ok=ok && arrayIsArray(v.value) && v.value.every(x=>typeof x==='string');
      else ok=false;
      if(soft && own(v,'availableOptions')) {
        ok=ok&&arrayIsArray(v.availableOptions)&&v.availableOptions.every(x=>typeof x==='string');
        if(ok)availableOptions.set(ref,v.availableOptions);
      }
      if(!ok){if(soft)return unavailable;missing=true;fail('SCRIPT_FACTOR_UNAVAILABLE','required factor unavailable: '+ref);}
      return v.value;
    }
    const factorTarget=Object.create(null);
    for(const ref of analysis.references)Object.defineProperty(factorTarget,ref,{get:()=>read(ref,false),enumerable:true});
    freeze(factorTarget);
    const factors=new Proxy(factorTarget,{get(target,key,receiver){if(!own(target,key)){missing=true;fail('SCRIPT_FACTOR_UNAVAILABLE','unknown required factor');}return reflectGet(target,key,receiver);}});
    const helpers=Object.freeze({
      all(children){let u=false;for(const child of children){const v=child();if(v===false)return false;if(v===unavailable)u=true;else if(v!==true)fail('SCRIPT_TYPE','invalid helper child');}return u?unavailable:true;},
      any(children){let u=false;for(const child of children){const v=child();if(v===true)return true;if(v===unavailable)u=true;else if(v!==false)fail('SCRIPT_TYPE','invalid helper child');}return u?unavailable:false;},
      test(ref,op,expected){const v=read(ref,true);if(v===unavailable)return v;
        // A resolver-supplied option catalog makes string choices outside it unavailable.
        const catalogApplies=op==='EQUALS'&&typeof expected==='string'||op==='IN'||op==='ANY_MATCH';
        if(catalogApplies && availableOptions.has(ref)) {
          const options=availableOptions.get(ref),requested=arrayIsArray(expected)?expected:[expected];
          if(requested.some(x=>!options.includes(x)))return unavailable;
        }
        if(op==='EXISTS')return typeof v==='boolean'?v:unavailable;
        if(op==='EQUALS'){if(arrayIsArray(v))return typeof expected==='string'?v.includes(expected):unavailable;return typeof v===typeof expected?v===expected:unavailable;}
        if(op==='GTE'||op==='LTE')return typeof v==='number'&&typeof expected==='number'?(op==='GTE'?v>=expected:v<=expected):unavailable;
        if(op==='IN'||op==='ANY_MATCH'){if(!arrayIsArray(expected))return unavailable;const list=arrayIsArray(v)?v:typeof v==='string'?[v]:null;return list?expected.some(x=>list.includes(x)):unavailable;}
        return unavailable;
      }
    });
    // Remove dynamic compilation and nondeterministic clocks before user code.
    delete Function.prototype.constructor;
    freeze(NativeFunction.prototype);
    globalThis.Function=undefined;globalThis.eval=undefined;globalThis.Date=undefined;
    globalThis.__hostRead=undefined;globalThis.__run=undefined;globalThis.acorn=undefined;
    Math.random=undefined;
    for(const ctor of [Object,Array,String,Number,Boolean,RegExp,Error,TypeError,RangeError,Map,Set,WeakMap,WeakSet,Promise,Proxy,Reflect,JSON,Math]) {
      if(ctor.prototype)Object.freeze(ctor.prototype);Object.freeze(ctor);
    }
    freeze(globalThis);
    let value;
    try { value=compiled(input,context,req.kind==='RULE'?factors:undefined,analysis.helper?helpers:undefined); }
    catch(e) {
      if(e && typeof e==='object' && !e.line && typeof e.stack==='string') {
        const location=regexpExec(/<input>:(\d+):(\d+)/,e.stack);
        if(location){e.line=maxNumber(1,numberFrom(location[1])-3);e.column=numberFrom(location[2]);}
      }
      throw e;
    }
    if(missing)fail('SCRIPT_REQUIRED_VALUE','a required input or factor was unavailable');
    if(value===unavailable)fail('SCRIPT_FACTOR_UNAVAILABLE','required visual condition unavailable');
    const valueType=typeof value==='string'?'TEXT':typeof value==='number'&&isFiniteNumber(value)?'NUMBER':typeof value==='boolean'?'BOOLEAN':null;
    const expected=req.kind==='RULE'?'BOOLEAN':req.returnType;
    if(!valueType || valueType!==expected)fail('SCRIPT_TYPE','script must return '+expected);
    if(stringify(value).length>65536)fail('SCRIPT_LIMIT','output exceeds limit');
    return {value,valueType};
  };
})();
