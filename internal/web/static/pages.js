// Registry of the pages that load their own data. A page module calls
// registerPage() when it is imported, and app.js does the rest:
//
//   registerPage('dashboard', {
//     scoped: false,              // true when the namespace selector applies (shown in the breadcrumb)
//     load: async (S) => data,    // fetch with S.ctx / S.ns; the result lands in S.load.data
//     html: (S) => string,        // S.load = { status: 'loading' | 'ready' | 'error', data, error, updatedAt }
//     onClick: async (act, el, app) => boolean,   // optional; return true when the click was handled
//     onInput:  (el, app) => boolean,             // optional
//     onChange: (el, app) => boolean,             // optional
//   });
//
// app = { S, navigate(view), setNamespace(ns), refresh(), rerender() }.

export const PAGES = {};

export function registerPage(id, page) {
  PAGES[id] = page;
}
