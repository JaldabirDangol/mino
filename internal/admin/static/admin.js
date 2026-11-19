// Mino admin: small client helpers and Alpine components. Loaded before Alpine (both deferred).
(function () {
  "use strict";
  const Mino = (window.Mino = {});

  // --- text helpers -----------------------------------------------------------------
  const ascii = (s) => (s || "").normalize("NFKD").replace(/[̀-ͯ]/g, "");
  Mino.slugify = (s) => ascii(s).toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "").slice(0, 60);
  Mino.camel = (s) => {
    const words = ascii(s).replace(/[^A-Za-z0-9]+/g, " ").trim().split(/\s+/).filter(Boolean);
    let out = words.map((w, i) => (i === 0 ? w.charAt(0).toLowerCase() + w.slice(1) : w.charAt(0).toUpperCase() + w.slice(1))).join("");
    if (out && !/^[a-zA-Z]/.test(out)) out = "f" + out;
    return out.slice(0, 60);
  };
  Mino.pluralize = (w) => {
    if (!w) return "";
    if (/[^aeiou]y$/.test(w)) return w.slice(0, -1) + "ies";
    if (/(s|x|z|ch|sh)$/.test(w)) return w + "es";
    return w + "s";
  };
  Mino.csrf = () => {
    try { return JSON.parse(document.body.getAttribute("hx-headers") || "{}")["X-CSRF-Token"] || ""; } catch { return ""; }
  };
  Mino.toast = (kind, message) => window.Alpine && Alpine.store("toasts").push(kind, message);
  Mino.confirm = (opts) => Alpine.store("confirm").ask(opts);

  // --- stores -----------------------------------------------------------------------
  document.addEventListener("alpine:init", () => {
    Alpine.store("toasts", {
      items: [],
      n: 0,
      push(kind, message) {
        const id = ++this.n;
        this.items.push({ id, kind, message });
        setTimeout(() => this.dismiss(id), kind === "error" ? 9000 : 4500);
      },
      dismiss(id) { this.items = this.items.filter((t) => t.id !== id); },
    });
    Alpine.store("confirm", {
      title: "", message: "", ok: "Confirm", danger: true, resolve: null,
      ask(opts) {
        this.title = opts.title || "Please confirm";
        this.message = opts.message || "";
        this.ok = opts.ok || "Confirm";
        this.danger = opts.danger !== false;
        document.getElementById("confirm-dialog").showModal();
        return new Promise((r) => (this.resolve = r));
      },
      answer(v) {
        const d = document.getElementById("confirm-dialog");
        if (d.open) d.close();
        const r = this.resolve;
        this.resolve = null;
        if (r) r(v);
      },
    });
  });

  // --- HTMX wiring ------------------------------------------------------------------
  if (window.htmx) {
    // Swap 422 responses (forms re-rendered with errors); never swap other errors.
    htmx.config.responseHandling = [
      { code: "204", swap: false },
      { code: "[23]..", swap: true },
      { code: "422", swap: true },
      { code: "[45]..", swap: false, error: true },
      { code: "...", swap: false },
    ];
    htmx.config.defaultSettleDelay = 0;
  }
  document.addEventListener("toast", (e) => e.detail && Mino.toast(e.detail.kind, e.detail.message));
  document.addEventListener("htmx:afterSwap", (e) => {
    if (e.detail.target && e.detail.target.id === "modal-body") {
      const d = document.getElementById("modal");
      if (d && !d.open) d.showModal();
    }
  });
  document.addEventListener("htmx:responseError", (e) => {
    if (!e.detail.xhr.getResponseHeader("HX-Trigger")) Mino.toast("error", "The request failed (" + e.detail.xhr.status + "). Reload and try again.");
  });
  document.addEventListener("htmx:sendError", () => Mino.toast("error", "Could not reach the server. Check your connection."));
  document.addEventListener("htmx:confirm", (e) => {
    if (!e.detail.question) return;
    e.preventDefault();
    Mino.confirm({ message: e.detail.question }).then((ok) => ok && e.detail.issueRequest(true));
  });

  // Forms (or submit buttons) with data-confirm ask before submitting.
  document.addEventListener("submit", (e) => {
    const form = e.target;
    const el = e.submitter && e.submitter.dataset.confirm ? e.submitter : form;
    if (!el.dataset || !el.dataset.confirm) return;
    if (form.dataset.confirmed === "1") { delete form.dataset.confirmed; return; }
    e.preventDefault();
    const submitter = e.submitter;
    Mino.confirm({ message: el.dataset.confirm, ok: el.dataset.confirmOk, danger: !("confirmSafe" in el.dataset) }).then((ok) => {
      if (!ok) return;
      form.dataset.confirmed = "1";
      submitter ? form.requestSubmit(submitter) : form.requestSubmit();
    });
  }, true);

  document.addEventListener("DOMContentLoaded", () => {
    document.querySelectorAll("dialog[data-open-on-load]").forEach((d) => d.showModal());
  });

  // --- rich text --------------------------------------------------------------------
  Mino.richText = (root, input, opts) => {
    const mount = () => window.MinoRichText.mount(root, input, opts);
    if (window.MinoRichText) mount();
    else window.addEventListener("mino:editor-ready", mount, { once: true });
  };

  // --- link fields ------------------------------------------------------------------
  const linkList = (cfg) => ({
    name: cfg.name,
    multiple: cfg.multiple,
    items: cfg.items || [],
    open: false,
    add(it) {
      if (this.items.some((x) => x.id === it.id)) return;
      this.items = this.multiple ? [...this.items, it] : [it];
      this.changed();
    },
    remove(i) { this.items.splice(i, 1); this.changed(); },
    move(i, d) {
      const j = i + d;
      if (j < 0 || j >= this.items.length) return;
      const a = [...this.items];
      [a[i], a[j]] = [a[j], a[i]];
      this.items = a;
      this.changed();
    },
    changed() { this.$root.dispatchEvent(new Event("change", { bubbles: true })); },
  });
  Mino.relationField = linkList;
  Mino.mediaField = linkList;

  // --- content type names -----------------------------------------------------------
  Mino.typeNames = (cfg) => ({
    name: cfg.name || "",
    uid: cfg.uid || "",
    plural: cfg.plural || "",
    touched: !!cfg.uid,
    sync() {
      if (this.touched) return;
      this.uid = Mino.slugify(this.name);
      this.plural = Mino.pluralize(this.uid);
    },
  });

  // --- Content-Type Builder ---------------------------------------------------------
  const glyphs = { string: "Aa", text: "¶", richtext: "Rt", number: "12", boolean: "0/1", date: "D", datetime: "DT", email: "@", enumeration: "≡", json: "{}", media: "Img", relation: "↔" };
  const reserved = ["id", "createdat", "updatedat", "publishedat", "createdby", "updatedby", "documentid", "locale"];
  const relationLabels = { oneToOne: "One-to-one", oneToMany: "One-to-many", manyToOne: "Many-to-one", manyToMany: "Many-to-many" };

  function cleanField(f) {
    const out = {};
    for (const [k, v] of Object.entries(f)) {
      if (k === "_key" || v === null || v === undefined || v === "" || v === false) continue;
      if (Array.isArray(v) && v.length === 0) continue;
      out[k] = v;
    }
    return out;
  }

  Mino.builder = (cfg) => ({
    ct: cfg.ct,
    targets: cfg.targets || [],
    types: cfg.types,
    saveUrl: cfg.saveUrl,
    original: "",
    key: 0,
    editing: null,
    editingIndex: null,
    tab: "basic",
    nameTouched: false,
    fieldError: "",
    enumText: "",
    defaultText: "",
    draft: {},
    plan: [],
    planBlocked: false,
    errors: [],
    saving: false,
    relationKinds: [
      { value: "manyToOne", label: "Many-to-one", help: (a, b) => `Each ${a} links one ${b}. A ${b} can be shared.` },
      { value: "manyToMany", label: "Many-to-many", help: (a, b) => `Each ${a} links any number of ${b} entries, and they can be shared.` },
      { value: "oneToOne", label: "One-to-one", help: (a, b) => `Each ${a} links one ${b}, used by no other ${a}.` },
      { value: "oneToMany", label: "One-to-many", help: (a, b) => `Each ${a} links several ${b} entries, each used by one ${a} only.` },
    ],

    init() {
      this.ct.fields = (this.ct.fields || []).map((f) => ({ ...f, _key: ++this.key }));
      this.original = this.serialize();
      window.addEventListener("beforeunload", (e) => { if (this.dirty) { e.preventDefault(); e.returnValue = ""; } });
    },
    get dirty() { return this.serialize() !== this.original; },
    payload() { return { ...this.ct, fields: this.ct.fields.map(cleanField) }; },
    serialize() { return JSON.stringify(this.payload()); },

    glyph(t) { return glyphs[t] || "?"; },
    typeLabel(t) { const i = this.types.find((x) => x.Type === t); return i ? i.Label : t; },
    targetName(uid) { const t = this.targets.find((x) => x.uid === (uid || (this.editing && this.editing.target))); return t ? t.name : "entry"; },
    relTargets() { return this.targets; },
    describe(f) {
      let s = this.typeLabel(f.type);
      if (f.type === "relation") s += ` · ${relationLabels[f.relation] || ""} → ${this.targetName(f.target)}`;
      if (f.type === "enumeration") s += ` · ${(f.enum || []).length} values`;
      if (f.type === "media") s += f.multiple ? " · multiple files" : " · single file";
      if (f.type === "number") s += ` · ${f.format || "decimal"}`;
      if (f.description) s += ` — ${f.description}`;
      return s;
    },
    canUnique(t) { return ["string", "text", "email", "number", "date", "datetime"].includes(t); },
    hasDefault(t) { return !["media", "relation", "json", "richtext"].includes(t); },
    enumValues() { return this.enumText.split("\n").map((v) => v.trim()).filter(Boolean); },

    newField() {
      this.editing = { type: "" };
      this.editingIndex = null;
      this.fieldError = "";
      this.$refs.fieldDialog.showModal();
    },
    pickType(type) {
      const f = { type, name: "", label: "", description: "", required: false, unique: false, private: false, readOnly: false };
      if (type === "number") f.format = "integer";
      if (type === "media") { f.multiple = false; f.allowedTypes = []; }
      if (type === "relation") { f.relation = "manyToOne"; f.target = ""; }
      this.editing = f;
      this.tab = "basic";
      this.nameTouched = false;
      this.enumText = "";
      this.defaultText = "";
    },
    editField(i) {
      const f = JSON.parse(JSON.stringify(this.ct.fields[i]));
      if (f.type === "media" && !f.allowedTypes) f.allowedTypes = [];
      this.editing = f;
      this.editingIndex = i;
      this.tab = "basic";
      this.nameTouched = true;
      this.fieldError = "";
      this.enumText = (f.enum || []).join("\n");
      this.defaultText = f.default === undefined || f.default === null ? "" : String(f.default);
      this.$refs.fieldDialog.showModal();
    },
    autoName() { if (!this.nameTouched) this.editing.name = Mino.camel(this.editing.label); },
    commitField() {
      const f = this.editing;
      f.name = (f.name || "").trim();
      const others = this.ct.fields.filter((_, i) => i !== this.editingIndex);
      if (!f.name) this.fieldError = "Enter a name";
      else if (!/^[a-zA-Z][a-zA-Z0-9_]*$/.test(f.name)) this.fieldError = "Use letters, digits and underscores, starting with a letter";
      else if (reserved.includes(f.name.toLowerCase())) this.fieldError = `"${f.name}" is reserved`;
      else if (others.some((o) => o.name.toLowerCase() === f.name.toLowerCase())) this.fieldError = "Another field has this name";
      else this.fieldError = "";
      if (this.fieldError) { this.tab = "basic"; return; }
      if (f.type === "relation" && !f.target) { this.fieldError = "Choose the related content type"; this.tab = "basic"; return; }
      if (f.type === "enumeration") {
        f.enum = this.enumValues();
        if (!f.enum.length) { this.fieldError = "Add at least one value"; this.tab = "basic"; return; }
      }
      if (this.hasDefault(f.type)) {
        const d = this.defaultText;
        if (d === "") delete f.default;
        else if (f.type === "number") f.default = Number(d);
        else if (f.type === "boolean") f.default = d === "true";
        else f.default = d;
      } else delete f.default;
      for (const k of ["minLength", "maxLength", "min", "max"]) {
        if (f[k] === "" || f[k] === null || Number.isNaN(f[k])) delete f[k];
      }
      if (!this.canUnique(f.type)) f.unique = false;
      if (this.editingIndex === null) this.ct.fields.push({ ...f, _key: ++this.key });
      else this.ct.fields.splice(this.editingIndex, 1, { ...f, _key: this.ct.fields[this.editingIndex]._key });
      this.$refs.fieldDialog.close();
    },
    removeField(i) {
      const f = this.ct.fields[i];
      Mino.confirm({ title: `Remove “${f.name}”?`, message: "The field is removed when you save. If entries have values for it, you will see how many before anything is deleted.", ok: "Remove field" })
        .then((ok) => ok && this.ct.fields.splice(i, 1));
    },
    move(i, d) {
      const j = i + d;
      if (j < 0 || j >= this.ct.fields.length) return;
      const a = [...this.ct.fields];
      [a[i], a[j]] = [a[j], a[i]];
      this.ct.fields = a;
    },
    openSettings() {
      const { displayName, uid, pluralName, description, kind, draftAndPublish } = this.ct;
      this.draft = { displayName, uid, pluralName, description: description || "", kind, draftAndPublish };
      this.$refs.settingsDialog.showModal();
    },
    commitSettings() {
      Object.assign(this.ct, this.draft);
      this.$refs.settingsDialog.close();
    },
    describeError(e) {
      const p = e.path || [];
      if (p[0] === "fields" && this.ct.fields[+p[1]]) return `Field “${this.ct.fields[+p[1]].name}”: ${e.message}`;
      const names = { displayName: "Display name", pluralName: "Plural API id", uid: "API id", kind: "Kind" };
      return (names[p[0]] || p.join(".")) + ": " + e.message;
    },
    async save(confirm) {
      this.saving = true;
      this.errors = [];
      try {
        const res = await fetch(this.saveUrl, {
          method: "POST",
          headers: { "Content-Type": "application/json", "X-CSRF-Token": Mino.csrf() },
          body: JSON.stringify({ contentType: this.payload(), confirm }),
        });
        const data = await res.json().catch(() => ({}));
        if (res.ok) {
          this.original = this.serialize();
          window.location.href = data.redirect;
          return;
        }
        if (res.status === 409) {
          this.plan = data.plan || [];
          this.planBlocked = !!data.blocked;
          this.$refs.planDialog.showModal();
        } else if (data.errors && data.errors.length) {
          this.errors = data.errors.map((e) => this.describeError(e));
        } else {
          this.errors = [data.message || `Saving failed (${res.status}).`];
        }
      } catch {
        this.errors = ["Could not reach the server. Your changes are still here; try again."];
      } finally {
        this.saving = false;
      }
    },
  });
})();
