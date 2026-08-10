// A minimal Cinnamon/GJS harness for loading applet.js under Node.
//
// The applet is GObject Introspection glue, so the bugs it harbours are typos
// and API misuse — the kind that only surface when Cinnamon actually loads it.
// This harness stands in for the platform so those show up in CI instead of on
// a panel.
//
// These are test doubles for a desktop toolkit that cannot run headless, not
// stand-ins for go-sysmon's own code: every line of applet.js executes for
// real against them.

const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");

const APPLET_PATH = path.join(
    __dirname, "..", "..", "cinnamon", "sysmon-go@sysmon", "applet.js"
);
const SHARED_DIR = path.join(__dirname, "..", "..", "shared");

/** recorder collects calls so a test can assert on what the applet did. */
function recorder() {
    const calls = [];
    const fn = function (...args) {
        calls.push(args);
        return fn.returns;
    };
    fn.calls = calls;
    fn.returns = undefined;
    return fn;
}

// ---- St --------------------------------------------------------------------

/** FakeCairoContext records drawing operations instead of rasterising. */
class FakeCairoContext {
    constructor() {
        this.ops = [];
    }
    setSourceRGBA(...a) { this.ops.push(["setSourceRGBA", ...a]); }
    setOperator(op) { this.ops.push(["setOperator", op]); }
    paint() { this.ops.push(["paint"]); }
    rectangle(...a) { this.ops.push(["rectangle", ...a]); }
    fill() { this.ops.push(["fill"]); }
    moveTo(...a) { this.ops.push(["moveTo", ...a]); }
    showText(text) { this.ops.push(["showText", text]); }
    selectFontFace(...a) { this.ops.push(["selectFontFace", ...a]); }
    setFontSize(size) { this.ops.push(["setFontSize", size]); }
}

class FakeDrawingArea {
    constructor() {
        this.handlers = {};
        this.repaints = 0;
        this.width = 0;
        this.height = 0;
        this.context = new FakeCairoContext();
        this.surfaceSize = [100, 22];
    }
    connect(signal, handler) { this.handlers[signal] = handler; }
    queue_repaint() { this.repaints++; }
    get_context() { return this.context; }
    get_surface_size() { return this.surfaceSize; }
    set_width(w) { this.width = w; }
    set_height(h) { this.height = h; }

    /** repaint drives the applet's repaint handler as Clutter would. */
    repaint() {
        this.context = new FakeCairoContext();
        this.handlers["repaint"](this);
        return this.context;
    }
}

// ---- PopupMenu -------------------------------------------------------------

class FakeLabel {
    constructor(text) { this.text = text || ""; }
    set_text(text) { this.text = text; }
    get_text() { return this.text; }
}

class FakeActor {
    constructor() { this.styleClasses = []; }
    add_actor() {}
    add_style_class_name(name) { this.styleClasses.push(name); }
}

class FakeMenuItem {
    constructor(text, params) {
        this.label = new FakeLabel(text);
        this.params = params || {};
        this.actor = new FakeActor();
        this.handlers = {};
    }
    connect(signal, handler) { this.handlers[signal] = handler; }
    activate() {
        if (this.handlers["activate"]) {
            this.handlers["activate"](this);
        }
    }
}

class FakeSeparator {
    constructor() {
        this.actor = new FakeActor();
        this.isSeparator = true;
    }
}

class FakeMenuBase {
    constructor() {
        this.items = [];
        this.isOpen = false;
    }
    get numMenuItems() { return this.items.length; }
    addMenuItem(item, position) {
        if (position === undefined || position === null || position >= this.items.length) {
            this.items.push(item);
        } else {
            this.items.splice(Math.max(0, position), 0, item);
        }
    }
    removeAll() { this.items = []; }
    toggle() { this.isOpen = !this.isOpen; }
}

class FakeSubMenuMenuItem extends FakeMenuItem {
    constructor(text) {
        super(text);
        this.menu = new FakeMenuBase();
    }
}

// ---- Soup / Gio ------------------------------------------------------------

/**
 * FakeStream serves scripted lines to read_line_async, then end-of-stream.
 * Callbacks fire synchronously, which keeps tests deterministic.
 */
class FakeStream {
    constructor(lines, keepOpen) {
        this.lines = lines ? lines.slice() : [];
        this.closed = false;
        this.readError = null;
        // keepOpen models a live stream: once the script is exhausted the read
        // stays outstanding instead of reporting end-of-stream, which is what
        // a server that is still running actually does.
        this.keepOpen = keepOpen === true;
    }

    read_line_async(priority, cancellable, callback) {
        if (this.closed) {
            return;
        }
        this.pendingCallback = callback;
        if (this.lines.length > 0 || !this.keepOpen) {
            this.deliver();
        }
    }

    /** deliver satisfies the outstanding read. */
    deliver() {
        const callback = this.pendingCallback;
        if (!callback) {
            return;
        }
        this.pendingCallback = null;
        callback(this, { line: this.lines.length > 0 ? this.lines.shift() : null });
    }

    read_line_finish_utf8(result) {
        if (this.readError) {
            throw this.readError;
        }
        return [result.line, result.line === null ? 0 : result.line.length];
    }

    close() { this.closed = true; }

    /** push appends a line and delivers it if a read is outstanding. */
    push(line) {
        this.lines.push(line);
        if (this.pendingCallback) {
            this.deliver();
        }
    }
}

/**
 * makeSandbox builds the global object applet.js executes against.
 *
 * @param {object} options
 * @param {Array<string>} [options.lines] - event-stream lines to serve
 * @param {boolean} [options.keepOpen] - leave the stream open after the script
 * @param {number} [options.status] - HTTP status the stream responds with
 * @param {boolean} [options.connectFails] - make send_async throw
 * @returns {object} the sandbox, with a `harness` field holding the doubles
 */
function makeSandbox(options) {
    const opts = options || {};
    const harness = {
        streams: [],
        timeouts: [],
        removedTimeouts: [],
        requests: [],
        spawned: [],
        settingsBindings: {},
        nextTimeoutId: 1,
    };

    const Soup = {
        Status: { OK: 200 },
        Message: {
            new(method, uri) {
                const message = {
                    method: method,
                    uri: uri,
                    headers: {},
                    body: null,
                    request_headers: {
                        append(name, value) { message.headers[name] = value; },
                    },
                    get_status() { return opts.status === undefined ? 200 : opts.status; },
                    set_request_body_from_bytes(contentType, bytes) {
                        message.body = { contentType: contentType, bytes: bytes };
                    },
                };
                harness.requests.push(message);
                return message;
            },
        },
        Session: class {
            send_async(message, priority, cancellable, callback) {
                if (opts.connectFails) {
                    throw new Error("connection refused");
                }
                this._message = message;
                // Only the event stream gets a scripted body; the interval POST
                // is fire-and-forget.
                if (String(message.uri).indexOf("/api/events") !== -1) {
                    const stream = new FakeStream(opts.lines, opts.keepOpen);
                    harness.streams.push(stream);
                    this._stream = stream;
                }
                callback(this, { ok: true });
            }
            send_finish() {
                if (opts.sendFinishFails) {
                    throw new Error("stream failed");
                }
                return this._stream || {};
            }
        },
    };

    const Gio = {
        Cancellable: class {
            constructor() { this.cancelled = false; }
            cancel() { this.cancelled = true; }
        },
        // The applet wraps the response body; the fake body already speaks the
        // DataInputStream interface, so pass it straight through.
        DataInputStream: class {
            constructor(params) { return params.base_stream; }
        },
    };

    const GLib = {
        PRIORITY_DEFAULT: 0,
        Bytes: class {
            constructor(data) { this.data = data; }
        },
        spawn_command_line_async(command) { harness.spawned.push(command); },
    };

    const Mainloop = {
        timeout_add(ms, fn) {
            const id = harness.nextTimeoutId++;
            harness.timeouts.push({ id: id, ms: ms, fn: fn });
            return id;
        },
        source_remove(id) { harness.removedTimeouts.push(id); },
    };

    const Applet = {
        // Cinnamon's applet base classes come from GJS's legacy class system,
        // where construction dispatches to _init rather than to a constructor.
        // Subclasses rely on that, so the stand-in has to do the same.
        TextIconApplet: class {
            constructor(...args) {
                this._init(...args);
            }
            _init(orientation, panelHeight, instanceId) {
                this._orientation = orientation;
                this._panelHeight = panelHeight;
                this._instanceId = instanceId;
                this.actor = new FakeActor();
                this.tooltip = "";
            }
            set_applet_tooltip(text) { this.tooltip = text; }
        },
        AppletPopupMenu: class extends FakeMenuBase {
            constructor(launcher, orientation) {
                super();
                this.launcher = launcher;
                this.orientation = orientation;
            }
        },
    };

    const PopupMenu = {
        PopupMenuManager: class {
            constructor(owner) { this.owner = owner; this.menus = []; }
            addMenu(menu) { this.menus.push(menu); }
        },
        PopupMenuItem: FakeMenuItem,
        PopupSubMenuMenuItem: FakeSubMenuMenuItem,
        PopupSeparatorMenuItem: FakeSeparator,
    };

    const Settings = {
        AppletSettings: class {
            constructor(owner, uuid, instanceId) {
                this.owner = owner;
                this.uuid = uuid;
                this.instanceId = instanceId;
                this.finalized = false;
            }
            bind(key, property, callback) {
                harness.settingsBindings[key] = { property: property, callback: callback };
                // Cinnamon assigns the stored value onto the owner; the
                // defaults here mirror settings-schema.json.
                const defaults = {
                    "server-address": "localhost:8080",
                    "update-interval": 1000,
                    "show-cpu": true,
                    "show-memory": true,
                    "show-network": true,
                    "show-disk": true,
                };
                this.owner[property] = defaults[key];
            }
            finalize() { this.finalized = true; }
        },
    };

    const imports = {
        ui: { applet: Applet, popupMenu: PopupMenu, settings: Settings },
        gi: { St: { DrawingArea: FakeDrawingArea }, Soup: Soup, Gio: Gio, GLib: GLib },
        mainloop: Mainloop,
        lang: {
            bind(obj, fn) { return fn.bind(obj); },
        },
    };

    const sandbox = {
        imports: imports,
        harness: harness,
        console: console,
        // Cinnamon resolves an applet's require() against the applet directory
        // and rejects anything with a subdirectory in it, so the shared modules
        // are installed alongside applet.js. Reject a subdirectory here too,
        // or this harness would accept a path Cinnamon will not.
        require(spec) {
            const name = String(spec).replace(/^\.\//, "");
            if (name.indexOf("/") !== -1) {
                throw new Error("[requireModule] Path does not exist: " + spec);
            }
            return require(path.join(SHARED_DIR, name));
        },
        JSON: JSON,
        Math: Math,
        String: String,
        Number: Number,
        isNaN: isNaN,
        parseInt: parseInt,
    };

    return sandbox;
}

/**
 * loadApplet executes applet.js in a fresh sandbox and returns both the
 * constructed applet and the harness doubles behind it.
 *
 * @param {object} [options] - see makeSandbox
 * @returns {{applet: object, harness: object}}
 */
function loadApplet(options) {
    const source = fs.readFileSync(APPLET_PATH, "utf8");
    const sandbox = makeSandbox(options);
    vm.createContext(sandbox);
    vm.runInContext(source, sandbox, { filename: APPLET_PATH });

    const metadata = { uuid: "sysmon-go@sysmon" };
    const applet = sandbox.main(metadata, "horizontal", 22, "instance-1");

    return { applet: applet, harness: sandbox.harness, sandbox: sandbox };
}

/** eventLines renders a snapshot as the lines the server would send. */
function eventLines(snapshot) {
    return ["retry: 3000", "", "event: snapshot", "data: " + JSON.stringify(snapshot), ""];
}

module.exports = { loadApplet, eventLines, recorder, APPLET_PATH };
