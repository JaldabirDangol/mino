// Copy buttons on code blocks and highlighting of the current section in the page index.
(() => {
  for (const pre of document.querySelectorAll(".prose pre")) {
    const btn = document.createElement("button");
    btn.type = "button";
    btn.className = "copy";
    btn.textContent = "Copy";
    btn.addEventListener("click", async () => {
      try {
        await navigator.clipboard.writeText(pre.querySelector("code")?.innerText ?? pre.innerText);
        btn.textContent = "Copied";
      } catch {
        btn.textContent = "Copy failed";
      }
      setTimeout(() => (btn.textContent = "Copy"), 1500);
    });
    pre.appendChild(btn);
  }

  const links = new Map([...document.querySelectorAll(".toc a")].map((a) => [a.hash.slice(1), a]));
  if (!links.size || !("IntersectionObserver" in window)) return;
  const visible = new Set();
  const io = new IntersectionObserver((entries) => {
    for (const e of entries) e.isIntersecting ? visible.add(e.target.id) : visible.delete(e.target.id);
    const first = [...links.keys()].find((id) => visible.has(id));
    if (!first) return;
    for (const [id, a] of links) a.classList.toggle("active", id === first);
  }, { rootMargin: "-70px 0px -60% 0px" });
  for (const id of links.keys()) {
    const h = document.getElementById(id);
    if (h) io.observe(h);
  }
})();
