/* Firefox and Safari cannot style the open list of a <select> (no appearance: base-select),
   so the list opens as a menu in the app's style instead. The <select> stays the source
   of truth: it shows the value and fires input and change. Phones and tablets keep the
   native picker, which suits a finger better. */
if (!CSS.supports("appearance", "base-select") && !matchMedia("(pointer: coarse)").matches) {
  const menu = document.createElement("div");
  menu.className = "pop-menu select-menu";
  menu.popover = "auto";
  menu.role = "listbox";
  let sel = null; // the select whose list is open
  let wasOpen = false; // a click on the select that just closed its list must not reopen it

  const items = () => [...menu.children];
  const pickable = (s) => s?.tagName === "SELECT" && !s.multiple && s.size <= 1 && !s.disabled;
  const close = () => menu.matches(":popover-open") && menu.hidePopover();

  function open(s) {
    sel = s;
    menu.replaceChildren(...[...s.options].map((o) => {
      const it = document.createElement("div");
      it.role = "option";
      it.tabIndex = -1;
      it.textContent = o.label;
      it.ariaSelected = String(o.selected);
      if (o.disabled) it.ariaDisabled = "true";
      return it;
    }));
    menu.ariaLabel = s.ariaLabel || s.labels[0]?.textContent.trim() || "";
    // Inside the popover or dialog holding the select, so opening the list keeps that one open.
    (s.closest("[popover], dialog") || document.body).append(menu);
    menu.showPopover();
    const r = s.getBoundingClientRect();
    menu.style.minWidth = r.width + "px";
    const below = innerHeight - r.bottom, above = r.top;
    const down = below >= menu.scrollHeight + 12 || below >= above;
    menu.style.maxHeight = (down ? below : above) - 12 + "px";
    menu.style.top = down ? r.bottom + 4 + "px" : "auto";
    menu.style.bottom = down ? "auto" : innerHeight - r.top + 4 + "px";
    menu.style.left = Math.max(8, Math.min(r.left, innerWidth - menu.offsetWidth - 8)) + "px";
    (items()[s.selectedIndex] || items()[0])?.focus();
  }

  function pick(it) {
    const i = items().indexOf(it);
    if (i < 0 || it.ariaDisabled) return;
    close();
    if (sel.selectedIndex !== i) {
      sel.selectedIndex = i;
      sel.dispatchEvent(new Event("input", { bubbles: true }));
      sel.dispatchEvent(new Event("change", { bubbles: true }));
    }
  }

  // The native list opens on mousedown, so that is stopped; the menu opens on click, after
  // the popover light dismiss of the same press has run.
  document.addEventListener("mousedown", (e) => {
    const s = e.target.closest?.("select");
    if (e.button !== 0 || !pickable(s)) return;
    e.preventDefault();
    wasOpen = menu.matches(":popover-open") && sel === s;
  });
  document.addEventListener("click", (e) => {
    const s = e.target.closest?.("select");
    if (!pickable(s)) return;
    if (!wasOpen) {
      s.focus();
      open(s);
    }
    wasOpen = false;
  });
  // Keys that open a native list; the arrow keys alone still change the value in place.
  document.addEventListener("keydown", (e) => {
    if (pickable(e.target) && (e.key === " " || e.key === "F4" || (e.altKey && (e.key === "ArrowDown" || e.key === "ArrowUp")))) {
      e.preventDefault();
      open(e.target);
    }
  });

  menu.addEventListener("keydown", (e) => {
    const list = items(), i = list.indexOf(document.activeElement);
    const go = (j) => list[Math.max(0, Math.min(list.length - 1, j))]?.focus();
    if (e.key === "ArrowDown") go(i + 1);
    else if (e.key === "ArrowUp") go(i - 1);
    else if (e.key === "Home" || e.key === "PageUp") go(0);
    else if (e.key === "End" || e.key === "PageDown") go(list.length - 1);
    else if (e.key === "Enter" || e.key === " ") pick(list[i]);
    else if (e.key === "Tab") return close();
    else if (e.key.length === 1 && !e.ctrlKey && !e.metaKey && !e.altKey) {
      // Type-ahead: jump to the next item starting with the typed letter.
      const k = e.key.toLowerCase();
      [...list.slice(i + 1), ...list.slice(0, i + 1)].find((it) => it.textContent.trim().toLowerCase().startsWith(k))?.focus();
    } else return;
    e.preventDefault();
  });
  menu.addEventListener("click", (e) => pick(e.target.closest('[role="option"]')));
  menu.addEventListener("mousemove", (e) => e.target.closest('[role="option"]')?.focus({ preventScroll: true }));
  menu.addEventListener("toggle", (e) => {
    const a = document.activeElement;
    if (e.newState === "closed" && (!a || a === document.body || menu.contains(a))) sel?.focus();
  });
  // The menu is placed once when it opens, so it closes when the page moves under it.
  addEventListener("resize", close);
  addEventListener("scroll", (e) => menu.contains(e.target) || close(), true);
}
