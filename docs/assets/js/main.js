// Tiny progressive-enhancement layer. No build step, no dependencies.

// --- copy-to-clipboard on every <pre> block --------------------------------
(function () {
  var blocks = document.querySelectorAll("pre");
  blocks.forEach(function (pre) {
    if (pre.parentElement.classList.contains("code-wrap")) return;
    var wrap = document.createElement("div");
    wrap.className = "code-wrap";
    pre.parentNode.insertBefore(wrap, pre);
    wrap.appendChild(pre);

    var btn = document.createElement("button");
    btn.type = "button";
    btn.className = "code-copy";
    btn.setAttribute("aria-label", "Copy code");
    btn.textContent = "copy";
    wrap.appendChild(btn);

    btn.addEventListener("click", function () {
      var text = pre.innerText;
      if (!navigator.clipboard) return;
      navigator.clipboard.writeText(text).then(function () {
        btn.textContent = "copied";
        btn.classList.add("code-copy--copied");
        setTimeout(function () {
          btn.textContent = "copy";
          btn.classList.remove("code-copy--copied");
        }, 1400);
      }).catch(function () {
        btn.textContent = "failed";
        setTimeout(function () { btn.textContent = "copy"; }, 1400);
      });
    });
  });
})();

// --- auto-generated table of contents on doc pages ------------------------
(function () {
  var nav = document.getElementById("toc");
  if (!nav) return;
  var content = document.querySelector(".doc__content");
  if (!content) return;
  var headings = content.querySelectorAll("h2, h3");
  if (!headings.length) {
    nav.parentElement.style.display = "none";
    return;
  }
  headings.forEach(function (h) {
    if (!h.id) {
      h.id = (h.textContent || "")
        .toLowerCase()
        .replace(/[^a-z0-9\s-]/g, "")
        .replace(/\s+/g, "-")
        .replace(/-+/g, "-")
        .replace(/^-|-$/g, "");
    }
    var a = document.createElement("a");
    a.href = "#" + h.id;
    a.textContent = h.textContent;
    a.className = "toc__lvl-" + h.tagName.charAt(1);
    nav.appendChild(a);
  });

  // Scroll-spy: highlight the section currently in view.
  var links = Array.from(nav.querySelectorAll("a"));
  var byId = {};
  links.forEach(function (a) { byId[a.getAttribute("href").slice(1)] = a; });
  var observer = new IntersectionObserver(function (entries) {
    entries.forEach(function (entry) {
      var link = byId[entry.target.id];
      if (!link) return;
      if (entry.isIntersecting) {
        links.forEach(function (l) { l.classList.remove("toc__active"); });
        link.classList.add("toc__active");
      }
    });
  }, { rootMargin: "0px 0px -70% 0px", threshold: 0.1 });
  headings.forEach(function (h) { observer.observe(h); });
})();
