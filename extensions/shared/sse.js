// Server-sent events parsing for go-sysmon extensions.
//
// Browsers ship EventSource; GJS does not, so the applet reads the response
// body itself and feeds lines through this parser. Keeping the state machine
// here — free of any GObject Introspection import — is what makes it testable
// under Node.
//
// The implementation follows the dispatch rules from the HTML event-stream
// specification: fields accumulate until a blank line dispatches the event,
// lines beginning with a colon are comments, and a field with no colon is
// treated as having an empty value.

/**
 * Parser accumulates event-stream lines and emits complete events.
 */
class Parser {
    constructor() {
        this.reset();
        // retry carries the server's reconnect hint, in milliseconds, and
        // persists across events because the specification treats it as
        // connection state rather than event data.
        this.retryMs = 0;
    }

    /**
     * reset clears the fields accumulated for the event being built.
     */
    reset() {
        this._name = "";
        this._data = [];
        this._id = "";
        this._sawField = false;
    }

    /**
     * push feeds one line, without its trailing newline, into the parser.
     * @param {string} line - a single line from the response body
     * @returns {object|null} the dispatched event, or null if none is complete
     */
    push(line) {
        if (line === undefined || line === null) {
            return null;
        }

        // Strip a trailing carriage return so CRLF streams parse identically.
        let text = line;
        if (text.length > 0 && text.charAt(text.length - 1) === "\r") {
            text = text.slice(0, -1);
        }

        // A blank line dispatches whatever has accumulated.
        if (text === "") {
            if (!this._sawField) {
                return null;
            }
            const event = {
                // An event with no explicit name is a "message" event.
                name: this._name === "" ? "message" : this._name,
                data: this._data.join("\n"),
                id: this._id,
            };
            this.reset();
            return event;
        }

        // A leading colon marks a comment, which exists only to keep the
        // connection warm.
        if (text.charAt(0) === ":") {
            return null;
        }

        const colon = text.indexOf(":");
        let field;
        let value;
        if (colon === -1) {
            field = text;
            value = "";
        } else {
            field = text.slice(0, colon);
            value = text.slice(colon + 1);
            // A single leading space after the colon is part of the framing,
            // not the value.
            if (value.charAt(0) === " ") {
                value = value.slice(1);
            }
        }

        switch (field) {
            case "event":
                this._name = value;
                this._sawField = true;
                break;
            case "data":
                this._data.push(value);
                this._sawField = true;
                break;
            case "id":
                // The specification requires ignoring an id containing NUL.
                if (value.indexOf("\u0000") === -1) {
                    this._id = value;
                    this._sawField = true;
                }
                break;
            case "retry": {
                // Only a run of ASCII digits is a valid reconnect hint.
                if (value.length > 0 && /^[0-9]+$/.test(value)) {
                    this.retryMs = parseInt(value, 10);
                }
                break;
            }
            default:
                // Unknown fields are ignored.
                break;
        }

        return null;
    }
}

/**
 * parseAll runs a complete event-stream body through a parser and returns
 * every event it dispatches. It exists for tests and for callers that already
 * hold the whole body.
 * @param {string} body - a complete event-stream body
 * @returns {Array<object>} the dispatched events
 */
function parseAll(body) {
    const parser = new Parser();
    const events = [];
    if (!body) {
        return events;
    }
    const lines = body.split("\n");
    for (let i = 0; i < lines.length; i++) {
        const event = parser.push(lines[i]);
        if (event) {
            events.push(event);
        }
    }
    return events;
}

module.exports = { Parser, parseAll };
