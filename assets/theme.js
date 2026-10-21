/* Wmail theme. Loaded in <head>, before the stylesheet paints, so a dark page never
   flashes white. The choice ("light", "dark" or "system") stays in this browser. */
(() => {
  const osDark = matchMedia("(prefers-color-scheme: dark)");
  const saved = () => {
    try { return localStorage.getItem("wmail.theme") || "system"; } catch { return "system"; }
  };
  const apply = () => {
    const t = saved();
    document.documentElement.dataset.theme = t === "system" ? (osDark.matches ? "dark" : "light") : t;
  };
  osDark.addEventListener("change", apply);
  window.wmailTheme = {
    get: saved,
    set(t) {
      try { localStorage.setItem("wmail.theme", t); } catch {}
      apply();
    },
    // bind wires a <select> of light/dark/system to the saved choice.
    bind(select) {
      select.value = saved();
      select.addEventListener("change", () => window.wmailTheme.set(select.value));
    },
  };
  apply();
})();
