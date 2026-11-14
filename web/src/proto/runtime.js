// Minimal protobuf wire runtime for the generated capsula.web.v1 bindings.
//
// The browser schema uses only proto3 scalars, strings, enums, messages and
// maps of scalars/messages. That is deliberately a smaller surface than the
// framework protocol, so this runtime implements exactly that subset instead
// of shipping a general-purpose protobuf library. The generated schema in
// web_pb.js is produced from the compiled descriptor by web/protogen.
//
// Conventions: 64-bit integers are JS numbers (every value in this schema is
// far below 2^53); float fields are rounded with Math.fround on encode; maps
// are plain objects; repeated fields are arrays.

const VARINT = 0;
const FIXED64 = 1;
const LEN = 2;
const FIXED32 = 5;

const encoder = new TextEncoder("utf-8");
const decoder = new TextDecoder("utf-8");

/** @param {Uint8Array} bytes @param {number} pos @returns {[bigint, number]} */
function readVarint(bytes, pos) {
  let result = 0n;
  let shift = 0n;
  for (let i = 0; i < 10; i++) {
    if (pos >= bytes.length) throw new Error("truncated varint");
    const b = bytes[pos++];
    result |= BigInt(b & 0x7f) << shift;
    if ((b & 0x80) === 0) return [result, pos];
    shift += 7n;
  }
  throw new Error("varint overflow");
}

function varintLength(value) {
  let v = BigInt(value);
  let n = 1;
  while (v >= 0x80n) {
    v >>= 7n;
    n++;
  }
  return n;
}

function pushVarint(out, value) {
  let v = BigInt(value);
  if (v < 0n) v = BigInt.asUintN(64, v);
  while (v >= 0x80n) {
    out.push(Number((v & 0x7fn) | 0x80n));
    v >>= 7n;
  }
  out.push(Number(v));
}

function pushFixed32(out, value) {
  let v = Math.round(value) >>> 0;
  out.push(v & 0xff, (v >>> 8) & 0xff, (v >>> 16) & 0xff, (v >>> 24) & 0xff);
}

function pushFloat32(out, value) {
  const b = new Uint8Array(4);
  new DataView(b.buffer).setFloat32(0, value, true);
  out.push(b[0], b[1], b[2], b[3]);
}

function pushFixed64(out, value) {
  const b = new Uint8Array(8);
  const dv = new DataView(b.buffer);
  dv.setBigUint64(0, BigInt.asUintN(64, BigInt(value)), true);
  out.push(...b);
}

function pushDouble(out, value) {
  const b = new Uint8Array(8);
  new DataView(b.buffer).setFloat64(0, value, true);
  out.push(...b);
}

function readFixed32(bytes, pos) {
  const dv = new DataView(bytes.buffer, bytes.byteOffset + pos, 4);
  return [dv.getUint32(0, true), pos + 4];
}

function readFloat32(bytes, pos) {
  const dv = new DataView(bytes.buffer, bytes.byteOffset + pos, 4);
  return [dv.getFloat32(0, true), pos + 4];
}

function readDouble(bytes, pos) {
  const dv = new DataView(bytes.buffer, bytes.byteOffset + pos, 8);
  return [dv.getFloat64(0, true), pos + 8];
}

function readFixed64(bytes, pos) {
  const dv = new DataView(bytes.buffer, bytes.byteOffset + pos, 8);
  return [Number(dv.getBigUint64(0, true)), pos + 8];
}

function scalarSize(kind, value) {
  switch (kind) {
    case "bool":
      return varintLength(value ? 1 : 0);
    case "string": {
      const len = encoder.encode(value).length;
      return varintLength(len) + len;
    }
    case "int32":
    case "uint32":
    case "int64":
    case "uint64":
    case "enum":
      return varintLength(typeof value === "bigint" ? value : value);
    case "float":
      return 4;
    case "double":
      return 8;
    default:
      throw new Error("unsupported scalar kind " + kind);
  }
}

/** @param {Array} fields */
function fieldByNumber(fields, no) {
  for (const f of fields) if (f.no === no) return f;
  return undefined;
}

/** @param {Array} fields */
function encodeFields(out, fields, obj, schema) {
  for (const f of fields) {
    const value = obj[f.name];
    if (value === undefined || value === null) continue;
    if (f.kind === "message") {
      if (f.repeated) {
        for (const item of value) {
          const nested = encodeMessage(schema, f.type, item);
          pushVarint(out, (f.no << 3) | LEN);
          pushVarint(out, nested.length);
          for (const b of nested) out.push(b);
        }
        continue;
      }
      if (typeof value !== "object") continue;
      const nested = encodeMessage(schema, f.type, value);
      // An empty nested message still encodes its tag: oneof payloads such as
      // ClockCommand.stopwatch_start carry presence with zero body bytes.
      pushVarint(out, (f.no << 3) | LEN);
      pushVarint(out, nested.length);
      for (const b of nested) out.push(b);
      continue;
    }
    if (f.kind === "map") {
      const entries = Object.entries(value);
      for (const [k, v] of entries) {
        const entry = [];
        encodeMapEntry(entry, f, k, v, schema);
        pushVarint(out, (f.no << 3) | LEN);
        pushVarint(out, entry.length);
        for (const b of entry) out.push(b);
      }
      continue;
    }
    if (f.repeated) {
      for (const item of value) {
        pushKey(out, f.no, f.kind);
        writeScalar(out, f.kind, item);
      }
      continue;
    }
    // Proto3 default values are omitted on the wire.
    if (f.kind === "bool" && value === false) continue;
    if (f.kind === "string" && value === "") continue;
    if (isNumericKind(f.kind) && Number(value) === 0) continue;
    if (f.kind === "enum" && Number(value) === 0) continue;
    pushKey(out, f.no, f.kind);
    writeScalar(out, f.kind, value);
  }
}

function isNumericKind(kind) {
  return (
    kind === "int32" ||
    kind === "uint32" ||
    kind === "int64" ||
    kind === "uint64" ||
    kind === "float" ||
    kind === "double"
  );
}

function pushKey(out, no, kind) {
  switch (kind) {
    case "float":
      pushVarint(out, (no << 3) | FIXED32);
      break;
    case "double":
      pushVarint(out, (no << 3) | FIXED64);
      break;
    case "string":
    case "bytes":
      pushVarint(out, (no << 3) | LEN);
      break;
    default:
      pushVarint(out, (no << 3) | VARINT);
  }
}

function writeScalar(out, kind, value) {
  switch (kind) {
    case "bool":
      pushVarint(out, value ? 1 : 0);
      break;
    case "string": {
      const data = encoder.encode(value);
      pushVarint(out, data.length);
      for (const b of data) out.push(b);
      break;
    }
    case "int32":
    case "uint32":
    case "int64":
    case "uint64":
    case "enum":
      pushVarint(out, value);
      break;
    case "float":
      pushFloat32(out, value);
      break;
    case "double":
      pushDouble(out, value);
      break;
    default:
      throw new Error("unsupported scalar kind " + kind);
  }
}

function encodeMapEntry(out, field, key, value, schema) {
  if (field.keyKind === "string") {
    const data = encoder.encode(key);
    pushVarint(out, 1 << 3 | LEN);
    pushVarint(out, data.length);
    for (const b of data) out.push(b);
  } else {
    pushVarint(out, 1 << 3 | VARINT);
    pushVarint(out, Number(key));
  }
  if (field.valueKind === "message") {
    const nested = encodeMessage(schema, field.valueType, value);
    pushVarint(out, 2 << 3 | LEN);
    pushVarint(out, nested.length);
    for (const b of nested) out.push(b);
    return;
  }
  pushVarint(out, 2 << 3 | (field.valueKind === "float" ? FIXED32 : field.valueKind === "double" ? FIXED64 : field.valueKind === "string" ? LEN : VARINT));
  writeScalar(out, field.valueKind, value);
}

/** @param {number} wire @param {Uint8Array} bytes @param {number} pos */
function skipField(wire, bytes, pos) {
  switch (wire) {
    case VARINT:
      return readVarint(bytes, pos)[1];
    case FIXED64:
      return pos + 8;
    case LEN: {
      const [len, next] = readVarint(bytes, pos);
      return next + Number(len);
    }
    case FIXED32:
      return pos + 4;
    default:
      throw new Error("unsupported wire type " + wire);
  }
}

/** @param {Uint8Array} bytes @param {number} pos @param {number} len */
function slice(bytes, pos, len) {
  return bytes.subarray(pos, pos + len);
}

function readScalar(kind, wire, bytes, pos) {
  switch (kind) {
    case "bool": {
      const [v, next] = readVarint(bytes, pos);
      return [v !== 0n, next];
    }
    case "string": {
      const [len, next] = readVarint(bytes, pos);
      return [decoder.decode(slice(bytes, next, Number(len))), next + Number(len)];
    }
    case "int32": {
      const [v, next] = readVarint(bytes, pos);
      return [Number(BigInt.asIntN(32, v)), next];
    }
    case "uint32":
    case "enum": {
      const [v, next] = readVarint(bytes, pos);
      return [Number(BigInt.asUintN(32, v)), next];
    }
    case "int64": {
      const [v, next] = readVarint(bytes, pos);
      return [Number(BigInt.asIntN(64, v)), next];
    }
    case "uint64": {
      const [v, next] = readVarint(bytes, pos);
      return [Number(v), next];
    }
    case "float":
      return readFloat32(bytes, pos);
    case "double":
      return readDouble(bytes, pos);
    default:
      throw new Error("unsupported scalar kind " + kind);
  }
}

/** @param {object} schema @param {string} typeName @param {Uint8Array} bytes */
export function decodeMessage(schema, typeName, bytes) {
  const def = schema[typeName];
  if (!def) throw new Error("unknown message type " + typeName);
  const out = {};
  for (const f of def.fields) {
    if (f.kind === "map") out[f.name] = {};
    else if (f.repeated) out[f.name] = [];
    else if (f.kind === "message") out[f.name] = undefined;
    else if (f.kind === "string") out[f.name] = "";
    else if (f.kind === "bool") out[f.name] = false;
    else out[f.name] = 0;
  }
  decodeInto(schema, def, bytes, out);
  return out;
}

function decodeInto(schema, def, bytes, out) {
  let pos = 0;
  while (pos < bytes.length) {
    let key;
    [key, pos] = readVarint(bytes, pos);
    const no = Number(key >> 3n);
    const wire = Number(key & 7n);
    const f = fieldByNumber(def.fields, no);
    if (!f) {
      pos = skipField(wire, bytes, pos);
      continue;
    }
    if (f.kind === "message") {
      if (wire !== LEN) throw new Error("bad wire type for " + def.name + "." + f.name);
      const [len, next] = readVarint(bytes, pos);
      const msg = decodeMessage(schema, f.type, slice(bytes, next, Number(len)));
      if (f.repeated) out[f.name].push(msg);
      else out[f.name] = msg;
      pos = next + Number(len);
      continue;
    }
    if (f.kind === "map") {
      if (wire !== LEN) throw new Error("bad map wire type for " + f.name);
      const [len, next] = readVarint(bytes, pos);
      const entry = decodeMapEntry(schema, f, slice(bytes, next, Number(len)));
      out[f.name][entry[0]] = entry[1];
      pos = next + Number(len);
      continue;
    }
    let value;
    [value, pos] = readScalar(f.kind, wire, bytes, pos);
    if (f.repeated) out[f.name].push(value);
    else out[f.name] = value;
  }
}

function decodeMapEntry(schema, field, bytes) {
  let pos = 0;
  let key;
  let value = field.valueKind === "string" ? "" : field.valueKind === "bool" ? false : field.valueKind === "message" ? undefined : 0;
  while (pos < bytes.length) {
    let tag;
    [tag, pos] = readVarint(bytes, pos);
    const no = Number(tag >> 3n);
    const wire = Number(tag & 7n);
    if (no === 1) {
      [key, pos] = readScalar(field.keyKind, wire, bytes, pos);
    } else if (no === 2) {
      if (field.valueKind === "message") {
        const [len, next] = readVarint(bytes, pos);
        value = decodeMessage(schema, field.valueType, slice(bytes, next, Number(len)));
        pos = next + Number(len);
      } else {
        [value, pos] = readScalar(field.valueKind, wire, bytes, pos);
      }
    } else {
      pos = skipField(wire, bytes, pos);
    }
  }
  return [key, value];
}

/** @param {object} schema @param {string} typeName @param {object} obj */
export function encodeMessage(schema, typeName, obj) {
  const def = schema[typeName];
  if (!def) throw new Error("unknown message type " + typeName);
  const out = [];
  encodeFields(out, def.fields, obj || {}, schema);
  return new Uint8Array(out);
}

export const Wire = { VARINT, FIXED64, LEN, FIXED32 };
