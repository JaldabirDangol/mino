// Rich-text editor for Rich Text fields. Exposes window.MinoRichText.mount(root, input).
// The server sanitizes whatever HTML this produces, so the editor is a convenience, not a security boundary.
import { Editor } from "@tiptap/core";
import StarterKit from "@tiptap/starter-kit";

const buttons = [
  { cmd: "bold", label: "Bold", text: "B", cls: "font-bold", run: (e) => e.chain().focus().toggleBold().run() },
  { cmd: "italic", label: "Italic", text: "I", cls: "italic", run: (e) => e.chain().focus().toggleItalic().run() },
  { cmd: "code", label: "Inline code", text: "</>", cls: "font-mono text-xs", run: (e) => e.chain().focus().toggleCode().run() },
  { sep: true },
  { cmd: "heading", attrs: { level: 2 }, label: "Heading", text: "H2", run: (e) => e.chain().focus().toggleHeading({ level: 2 }).run() },
  { cmd: "heading", attrs: { level: 3 }, label: "Subheading", text: "H3", run: (e) => e.chain().focus().toggleHeading({ level: 3 }).run() },
  { cmd: "bulletList", label: "Bulleted list", text: "• List", run: (e) => e.chain().focus().toggleBulletList().run() },
  { cmd: "orderedList", label: "Numbered list", text: "1. List", run: (e) => e.chain().focus().toggleOrderedList().run() },
  { cmd: "blockquote", label: "Quote", text: "❝", run: (e) => e.chain().focus().toggleBlockquote().run() },
  { cmd: "link", label: "Link", text: "Link", run: (e) => {
      const prev = e.getAttributes("link").href || "";
      const url = window.prompt("Link address (leave empty to remove)", prev);
      if (url === null) return;
      if (url === "") return e.chain().focus().extendMarkRange("link").unsetLink().run();
      e.chain().focus().extendMarkRange("link").setLink({ href: url }).run();
    } },
  { sep: true },
  { label: "Undo", text: "↶", run: (e) => e.chain().focus().undo().run() },
  { label: "Redo", text: "↷", run: (e) => e.chain().focus().redo().run() },
];

function mount(root, input, opts = {}) {
  const toolbar = document.createElement("div");
  toolbar.className = "flex flex-wrap items-center gap-0.5 border-b border-base-300 bg-base-200/60 px-1.5 py-1";
  toolbar.setAttribute("role", "toolbar");
  const surface = document.createElement("div");
  root.append(toolbar, surface);

  const editor = new Editor({
    element: surface,
    extensions: [StarterKit.configure({ link: { openOnClick: false, autolink: true } })],
    content: input.value || "",
    editable: !opts.readOnly,
    editorProps: { attributes: { class: "prose-editor", "aria-label": opts.label || "Rich text" } },
    onUpdate: ({ editor }) => {
      input.value = editor.isEmpty ? "" : editor.getHTML();
      input.dispatchEvent(new Event("input", { bubbles: true }));
    },
  });

  const els = [];
  for (const b of buttons) {
    if (b.sep) {
      const s = document.createElement("span");
      s.className = "mx-0.5 h-5 w-px bg-base-300";
      toolbar.append(s);
      continue;
    }
    const el = document.createElement("button");
    el.type = "button";
    el.className = "btn btn-ghost btn-xs min-w-7 px-1.5 " + (b.cls || "");
    el.textContent = b.text;
    el.title = b.label;
    el.setAttribute("aria-label", b.label);
    el.disabled = !!opts.readOnly;
    el.addEventListener("click", () => b.run(editor));
    toolbar.append(el);
    els.push([el, b]);
  }
  const sync = () => {
    for (const [el, b] of els) {
      if (!b.cmd) continue;
      const active = editor.isActive(b.cmd, b.attrs);
      el.classList.toggle("btn-active", active);
      el.setAttribute("aria-pressed", active ? "true" : "false");
    }
  };
  editor.on("selectionUpdate", sync);
  editor.on("transaction", sync);
  return editor;
}

window.MinoRichText = { mount };
window.dispatchEvent(new Event("mino:editor-ready"));
