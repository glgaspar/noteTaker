// Applies the dark theme before the page paints (loaded without defer in <head>).
try {
  const t = localStorage.getItem("theme");
  if (t === "dark" || (t !== "light" && matchMedia("(prefers-color-scheme: dark)").matches)) document.documentElement.classList.add("dark");
} catch (e) {}
