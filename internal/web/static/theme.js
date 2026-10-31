// Runs before the first paint so a saved theme does not flash the other one.
(function () {
  try {
    var theme = localStorage.getItem('k8scli-theme');
    if (theme === 'dark' || theme === 'light') document.documentElement.setAttribute('data-theme', theme);
  } catch (e) { /* storage unavailable: follow the system */ }
})();
