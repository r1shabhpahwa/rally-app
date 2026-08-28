// Progressive enhancement only: every form below works without this file.
document.addEventListener('click', function (e) {
  var el = e.target.closest('[data-confirm]');
  if (el && !window.confirm(el.getAttribute('data-confirm'))) {
    e.preventDefault();
  }
});

// Guest stepper: keeps the hidden field and the visible count in sync.
document.querySelectorAll('[data-stepper]').forEach(function (box) {
  var field = box.querySelector('input[name="guest_count"]');
  var out = box.querySelector('output');
  var max = parseInt(box.getAttribute('data-max') || '4', 10);
  if (!field || !out) { return; }
  // Only hide the plain number input once the stepper is known to work.
  box.classList.add('js-on');
  field.addEventListener('input', function () { out.textContent = field.value; });
  box.querySelectorAll('[data-step]').forEach(function (btn) {
    btn.addEventListener('click', function () {
      var next = parseInt(field.value || '0', 10) + parseInt(btn.getAttribute('data-step'), 10);
      field.value = Math.max(0, Math.min(max, next));
      out.textContent = field.value;
    });
  });
});
