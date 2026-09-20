/* epmon.dev status badge — tiny embeddable widget (no dependencies).
 *
 * Usage:
 *   <div data-epmon-badge data-slug="acme-inc"></div>
 *   <script src="https://epmon.dev/embed.js" data-api="https://epmon.dev" async></script>
 *
 * Attributes (on the <script> tag unless noted):
 *   data-api   Base URL of the status API (default: same origin as embed.js).
 *   data-slug  (on the badge div) Tenant slug, e.g. "acme-inc". Informational;
 *              the API resolves the tenant from the Host on custom domains.
 *   data-mock  (on the badge div) "operational" | "degraded" | "outage" —
 *              render a demo badge without any network request.
 *
 * Behavior: GET {api}/api/v1/status (Cache-Control: no-store), render a
 * dot + label, re-poll every 60s. Total script < 2KB.
 */
(function () {
  'use strict';

  var POLL_MS = 60000;

  var COLORS = {
    operational: '#16a34a',
    degraded: '#d97706',
    partial_outage: '#ea580c',
    major_outage: '#dc2626',
    unknown: '#6b7280',
  };

  var LABELS = {
    operational: 'Operational',
    degraded: 'Degraded',
    partial_outage: 'Partial outage',
    major_outage: 'Major outage',
    unknown: 'Unknown',
  };

  function baseUrl() {
    var s = document.currentScript;
    if (s && s.getAttribute('data-api')) return s.getAttribute('data-api');
    if (s && s.src) {
      try {
        return new URL(s.src).origin;
      } catch (e) {
        return '';
      }
    }
    return '';
  }

  function render(el, overall) {
    var color = COLORS[overall] || COLORS.unknown;
    var label = LABELS[overall] || LABELS.unknown;
    el.setAttribute('role', 'status');
    el.setAttribute('aria-live', 'polite');
    el.innerHTML =
      '<span style="display:inline-flex;align-items:center;gap:.4rem;' +
      'font:500 13px/1.4 system-ui,-apple-system,\'Segoe UI\',Roboto,sans-serif;' +
      'color:inherit">' +
      '<span aria-hidden="true" style="width:.65rem;height:.65rem;border-radius:9999px;' +
      'background:' + color + ';display:inline-block"></span>' +
      '<span></span></span>';
    el.querySelector('span span:last-child').textContent = label;
    el.title = 'epmon.dev status: ' + label;
  }

  function tick(el, api) {
    fetch(api + '/api/v1/status', { headers: { Accept: 'application/json' }, cache: 'no-store' })
      .then(function (r) {
        if (!r.ok) throw new Error('http ' + r.status);
        return r.json();
      })
      .then(function (d) {
        render(el, d && d.overall);
      })
      .catch(function () {
        render(el, 'unknown');
      });
  }

  function init() {
    var api = baseUrl();
    var badges = document.querySelectorAll('[data-epmon-badge]');
    for (var i = 0; i < badges.length; i++) {
      (function (el) {
        var mock = el.getAttribute('data-mock');
        if (mock) {
          render(el, mock === 'outage' ? 'major_outage' : mock);
          return;
        }
        tick(el, api);
        setInterval(function () {
          tick(el, api);
        }, POLL_MS);
      })(badges[i]);
    }
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
