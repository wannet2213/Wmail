const form = document.querySelector("#loginForm");
const errEl = document.querySelector("#loginError");
const btn = document.querySelector("#loginBtn");

form.addEventListener("submit", async (e) => {
  e.preventDefault();
  errEl.textContent = "";
  btn.disabled = true;
  try {
    const res = await fetch("/api/auth/login", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ username: form.username.value, password: form.password.value }),
    });
    if (!res.ok) throw new Error((await res.text()).trim() || "Gagal masuk.");
    location.href = "/";
  } catch (err) {
    errEl.textContent = err.message;
    form.password.select();
  } finally {
    btn.disabled = false;
  }
});
