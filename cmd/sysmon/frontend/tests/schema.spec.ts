import { test, expect } from '@playwright/test';
import {
  checkSnapshot,
  checkType,
  checkValue,
  validateSnapshot,
  REQUIRED_ARRAYS,
  REQUIRED_SECTIONS,
} from '../src/lib/schema';
import { SCHEMA } from '../src/lib/schema.generated';

/**
 * Frame validation, against the schema generated from the Go types (#38).
 *
 * These run in node rather than the browser: the module under test is plain
 * data and a walk over it, and putting it in front of a page would only add a
 * server, a bundle and thirty seconds to a test that needs none of them.
 *
 * The fixture below is the smallest snapshot the validator accepts. Every case
 * starts from it and breaks exactly one thing, so a failure names the rule that
 * broke rather than "the object was wrong somewhere".
 */
function validSnapshot(): Record<string, unknown> {
  const psi = { some_avg10: 0, some_avg60: 0, some_avg300: 0, full_avg10: 0, full_avg60: 0, full_avg300: 0 };
  return {
    timestamp: '2026-09-06T12:00:00Z',
    host: {},
    cpu_summary: {},
    memory: {},
    load_avg: {},
    processes: {},
    sensors: { psi: { cpu: psi, memory: psi, io: psi } },
    virtualization: { runtime: {}, capability: {} },
    cpus: [],
    disks: [],
    networks: [],
  };
}

test.describe('a well-formed frame is accepted', () => {
  test('the minimal snapshot passes', () => {
    expect(checkSnapshot(validSnapshot())).toBeNull();
  });

  test('a populated snapshot passes', () => {
    const snap = validSnapshot();
    snap.networks = [
      { index: 2, name: 'eno1', flags: ['up', 'broadcast'], is_up: true, bytes_sent_rate: 1.5 },
    ];
    snap.cpus = [{ index: 0, model_name: 'x', flags: ['sse'], usage_percent: 12.5 }];
    snap.disks = [{ name: 'nvme0n1', partitions: [{ name: 'nvme0n1p1' }] }];
    snap.gpus = null; // a machine with no GPU
    snap.alerts = [];
    expect(checkSnapshot(snap)).toBeNull();
  });

  test('validateSnapshot hands back the narrowed value', () => {
    const checked = validateSnapshot(validSnapshot());
    expect(checked.ok).toBe(true);
    if (checked.ok) {
      expect(checked.snapshot.timestamp).toBe('2026-09-06T12:00:00Z');
    }
  });
});

test.describe('the containers the page walks unguarded are required', () => {
  for (const section of REQUIRED_SECTIONS) {
    test(`${section} missing is refused`, () => {
      const snap = validSnapshot();
      const parts = section.split('.');
      let held: Record<string, unknown> = snap;
      for (const step of parts.slice(0, -1)) {
        held = held[step] as Record<string, unknown>;
      }
      delete held[parts[parts.length - 1]];

      const reason = checkSnapshot(snap);
      // sensors.psi is the one section allowed to vanish, taking its members
      // with it. Its members going missing while it stays is a different thing
      // and is still refused: psiRows reads some_avg10 off whatever it is
      // handed.
      if (section === 'sensors.psi') {
        expect(reason, 'a kernel with no pressure-stall information').toBeNull();
      } else {
        expect(reason).toContain(section);
      }
    });
  }

  for (const key of REQUIRED_ARRAYS) {
    test(`${key} as null is refused`, () => {
      const snap = validSnapshot();
      snap[key] = null;
      expect(checkSnapshot(snap)).toContain(key);
    });
  }
});

test.describe('the frame is refused when a value is the wrong kind', () => {
  test('a list of strings arriving as a string, inside an element', () => {
    const snap = validSnapshot();
    // The case from the issue: accepted before, and the page threw on .join().
    snap.networks = [{ name: 'eno1' }, { name: 'eno2', flags: 'x' }];
    expect(checkSnapshot(snap)).toBe('networks[1].flags: expected array, got string');
  });

  test('a number arriving as a string, inside an element', () => {
    const snap = validSnapshot();
    // Worse than throwing: this coerced through arithmetic and rendered as a
    // number while the page reported itself connected and current.
    snap.networks = [{ bytes_sent_rate: 'x' }];
    expect(checkSnapshot(snap)).toBe('networks[0].bytes_sent_rate: expected number, got string');
  });

  test('a null element in a list of objects', () => {
    const snap = validSnapshot();
    snap.gpus = [null];
    expect(checkSnapshot(snap)).toBe('gpus[0]: expected object, got null');
  });

  test('an array-valued field nested two levels down', () => {
    const snap = validSnapshot();
    snap.disks = [{ name: 'sda', partitions: 'x' }];
    expect(checkSnapshot(snap)).toBe('disks[0].partitions: expected array, got string');
  });

  test('a nested object arriving as an array', () => {
    const snap = validSnapshot();
    snap.disks = [{ name: 'sda', raid: [] }];
    expect(checkSnapshot(snap)).toBe('disks[0].raid: expected object, got array');
  });

  test('a boolean arriving as a number', () => {
    const snap = validSnapshot();
    snap.cpus = [{ index: 0 }];
    snap.networks = [{ is_up: 1 }];
    expect(checkSnapshot(snap)).toBe('networks[0].is_up: expected boolean, got number');
  });

  test('the envelope itself', () => {
    expect(checkSnapshot(null)).toBe('snapshot: expected object, got null');
    expect(checkSnapshot([])).toBe('snapshot: expected object, got array');
    expect(checkSnapshot(7)).toBe('snapshot: expected object, got number');
    expect(checkSnapshot({})).toBe('timestamp: expected string, got undefined');
  });
});

test.describe('what the frame is allowed to be missing, and why', () => {
  test('a key the type declares but the frame omits is not an error', () => {
    // This is what keeps the validator safe to make strict. A field renamed on
    // the Go side arrives under its new name and the old one is absent: one
    // panel renders undefined, in one cell, instead of every frame being
    // refused and the whole dashboard going blank.
    const snap = validSnapshot();
    snap.networks = [{ name: 'eno1' }];
    expect(checkSnapshot(snap)).toBeNull();
  });

  test('a key the type does not declare is ignored', () => {
    // A server that has grown a field must not be refused by an older page, or
    // every deploy is an outage on half the fleet.
    const snap = validSnapshot();
    snap.networks = [{ name: 'eno1', something_new: { nested: true } }];
    snap.something_new = 42;
    expect(checkSnapshot(snap)).toBeNull();
  });

  test('a nil Go slice arriving as null is a machine that has none', () => {
    const snap = validSnapshot();
    snap.gpus = null;
    snap.alerts = null;
    snap.networks = [{ addresses: null, flags: null }];
    expect(checkSnapshot(snap)).toBeNull();
  });

  test('an optional pointer arriving as null', () => {
    const snap = validSnapshot();
    snap.disks = [{ name: 'sda', raid: null }];
    snap.networks = [{ name: 'eno1', wireless: null }];
    expect(checkSnapshot(snap)).toBeNull();
  });

  test('sensors.psi absent, or explicitly null, on a kernel without it', () => {
    const absent = validSnapshot();
    absent.sensors = {};
    expect(checkSnapshot(absent)).toBeNull();

    const explicit = validSnapshot();
    explicit.sensors = { psi: null };
    expect(checkSnapshot(explicit)).toBeNull();
  });

  test('sensors.psi present but half-built is still refused', () => {
    // psiRows reads some_avg10 off whatever it is handed, so a psi with no cpu
    // is not the same thing as no psi at all.
    const snap = validSnapshot();
    snap.sensors = { psi: {} };
    expect(checkSnapshot(snap)).toContain('sensors.psi.cpu');
  });
});

test.describe('the refusal says where and what', () => {
  test('every reason names a path and both kinds', () => {
    const snap = validSnapshot();
    snap.networks = [{}, {}, { flags: 3 }];
    const reason = checkSnapshot(snap);
    expect(reason).toContain('networks[2].flags');
    expect(reason).toContain('expected array');
    expect(reason).toContain('got number');
  });

  test('validateSnapshot carries the same reason out', () => {
    const checked = validateSnapshot({ ...validSnapshot(), cpus: 'x' });
    expect(checked.ok).toBe(false);
    if (!checked.ok) {
      expect(checked.reason).toBe('cpus: expected array, got string');
    }
  });
});

test.describe('the walk itself', () => {
  test('checkValue accepts and refuses a scalar', () => {
    expect(checkValue('x', { kind: 'string' }, 'p')).toBeNull();
    expect(checkValue(1, { kind: 'string' }, 'p')).toBe('p: expected string, got number');
  });

  test('checkValue refuses null only where the schema forbids it', () => {
    expect(checkValue(null, { kind: 'array', elem: { kind: 'string' }, nullable: true }, 'p')).toBeNull();
    expect(checkValue(null, { kind: 'string' }, 'p')).toBe('p: expected string, got null');
  });

  test('checkType accepts and refuses against a named type', () => {
    expect(checkType({ name: 'eno1' }, 'NetworkInfo', 'n')).toBeNull();
    expect(checkType({ mtu: 'big' }, 'NetworkInfo', 'n')).toBe('n.mtu: expected number, got string');
  });

  test('checkType on a type the schema does not carry says so', () => {
    // Only reachable when the generated schema and the code naming a type were
    // built from different trees, which is a mismatch worth naming rather than
    // passing silently.
    expect(checkType({}, 'NotAType', 'p')).toBe('p: no schema for type NotAType');
  });
});

test.describe('the generated schema covers the tree', () => {
  test('every type an object field refers to is itself in the schema', () => {
    // A dangling ref would make checkType return "no schema for type X" on a
    // perfectly good frame, which is the one way this design can fail closed.
    const missing: string[] = [];
    const walk = (shape: { kind: string; ref?: string; elem?: unknown }) => {
      if (shape.kind === 'object' && shape.ref && !(shape.ref in SCHEMA)) missing.push(shape.ref);
      if (shape.kind === 'array' && shape.elem) walk(shape.elem as { kind: string; ref?: string });
    };
    for (const fields of Object.values(SCHEMA)) {
      for (const field of fields) walk(field.type);
    }
    expect(missing).toEqual([]);
  });

  test('the sections the policy requires are types the schema knows', () => {
    const snapshot = SCHEMA.Snapshot;
    expect(snapshot, 'the schema has no Snapshot').toBeDefined();
    for (const key of REQUIRED_ARRAYS) {
      const field = snapshot.find((f) => f.key === key);
      expect(field, `Snapshot declares no ${key}`).toBeDefined();
      expect(field!.type.kind).toBe('array');
    }
  });
});

/**
 * A list that arrives as null is handed on as [] (#126 review, #151).
 *
 * This build never sends a null list, but a peer on a build from before #151
 * still can during a rolling upgrade. The schema accepts that frame, and the
 * generated interfaces spell every list as `T[]` -- so an accepted null has to
 * become an empty list here, or a `.map` behind the type throws exactly as
 * #151 described.
 */
test.describe('accepted null lists are normalized to []', () => {
  test('a top-level list', () => {
    const snap = validSnapshot();
    snap.gpus = null;
    const checked = validateSnapshot(snap);
    expect(checked.ok).toBe(true);
    if (checked.ok) expect(checked.snapshot.gpus).toEqual([]);
  });

  test('a list inside a section', () => {
    const snap = validSnapshot();
    snap.memory = { dimms: null };
    const checked = validateSnapshot(snap);
    expect(checked.ok).toBe(true);
    if (checked.ok) expect(checked.snapshot.memory.dimms).toEqual([]);
  });

  test('a list inside a list element', () => {
    const snap = validSnapshot();
    snap.networks = [{ name: 'bond0', bond_slaves: null, addresses: null }];
    const checked = validateSnapshot(snap);
    expect(checked.ok).toBe(true);
    if (checked.ok) {
      expect(checked.snapshot.networks[0].bond_slaves).toEqual([]);
      expect(checked.snapshot.networks[0].addresses).toEqual([]);
    }
  });

  test('a null pointer section stays null', () => {
    // Only lists are normalized: a nil pointer is a real "absent" the page
    // branches on, and its interface says `| null`.
    const snap = validSnapshot();
    snap.disks = [{ name: 'sda', raid: null }];
    const checked = validateSnapshot(snap);
    expect(checked.ok).toBe(true);
    if (checked.ok) expect(checked.snapshot.disks[0].raid).toBeNull();
  });
});

test.describe('nested null lists are normalized too', () => {
  test('a null list inside a list becomes []', () => {
    // The generator accepts [][]T. A pre-#151 peer can send a nil inner slice,
    // and the generated type spells it T[][], so the accepted null must become
    // an empty list here as well.
    const nested = { kind: 'array', elem: { kind: 'array', elem: { kind: 'number' }, nullable: true }, nullable: true } as const;
    const value: unknown[] = [[1, 2], null, [3]];
    expect(checkValue(value, nested, 'grid')).toBeNull();
    expect(value).toEqual([[1, 2], [], [3]]);
  });

  test('a null element that is not a list is still refused', () => {
    const numbers = { kind: 'array', elem: { kind: 'number' }, nullable: true } as const;
    expect(checkValue([1, null], numbers, 'nums')).not.toBeNull();
  });
});

test.describe('required section paths resolve through the schema', () => {
  // A renamed Go key would leave a stale dotted path here, and checkSnapshot
  // would then refuse every frame. Every segment must still be an object field.
  for (const path of REQUIRED_SECTIONS) {
    test(`${path} resolves to object fields`, () => {
      let type = 'Snapshot';
      for (const segment of path.split('.')) {
        const fields = SCHEMA[type];
        expect(fields, `no schema for ${type} while resolving ${path}`).toBeDefined();
        const field = fields.find((f) => f.key === segment);
        expect(field, `${type} declares no ${segment} (path ${path})`).toBeDefined();
        expect(field!.type.kind, `${path}: ${segment} is not an object`).toBe('object');
        type = (field!.type as { ref: string }).ref;
      }
    });
  }
});
