// Generic chip-style multi-select.
//
// Each <div class="multiselect" data-name="<form-field>" data-source="<key>"
//                              data-selected="id1,id2" data-empty="Empty label">
// becomes a self-rendering control that shows selected options as colored
// pills and exposes an "Add" popover with type-to-filter for picking more.
// Hidden inputs named data-name carry the values to the server on submit.
//
// Options come from window.PRISHE_<UPPERCASE_SOURCE> - an array of
// {id, name, color?} objects. The dot is rendered only when color is set,
// so channels (no color) display cleanly without one.
(function () {
  function optionsFor(source) {
    return window['PRISHE_' + (source || '').toUpperCase()] || [];
  }

  // Close any open popover when clicking outside any multiselect.
  document.addEventListener('click', function (e) {
    document.querySelectorAll('.multiselect.open').forEach(function (el) {
      if (!el.contains(e.target)) el.classList.remove('open');
    });
  });

  document.querySelectorAll('.multiselect').forEach(function (root) {
    var name        = root.dataset.name;
    var source      = root.dataset.source;
    var emptyLabel  = root.dataset.empty || 'None selected';
    var addLabel    = root.dataset.addLabel || '+ Add';
    var initial     = (root.dataset.selected || '').split(',').filter(Boolean);
    // data-max="1" turns the control into a single-select: picking a new
    // option replaces the existing one and the popup closes immediately.
    // 0 / unset = unlimited.
    var max         = parseInt(root.dataset.max || '0', 10);

    var options = optionsFor(source);
    var byID = {};
    options.forEach(function (o) { byID[o.id] = o; });

    var selected = initial.filter(function (id) { return byID[id]; });

    // Markup scaffold.
    var pills    = document.createElement('div'); pills.className    = 'ms-pills';
    var control  = document.createElement('div'); control.className  = 'ms-control';
    var addBtn   = document.createElement('button'); addBtn.type     = 'button';
    addBtn.className = 'ms-add'; addBtn.textContent = addLabel;
    var popup    = document.createElement('div'); popup.className    = 'ms-popup';
    var filter   = document.createElement('input');
    filter.type  = 'search'; filter.className = 'ms-filter'; filter.placeholder = 'Filter…';
    var list     = document.createElement('ul'); list.className = 'ms-options';
    popup.appendChild(filter); popup.appendChild(list);
    control.appendChild(addBtn); control.appendChild(popup);
    root.appendChild(pills); root.appendChild(control);

    function makeDot(color) {
      if (!color) return null;
      var dot = document.createElement('span');
      dot.className = 'ms-dot';
      dot.style.background = color;
      return dot;
    }

    function render() {
      // Pills.
      pills.innerHTML = '';
      if (selected.length === 0) {
        var empty = document.createElement('span');
        empty.className = 'ms-empty';
        empty.textContent = emptyLabel;
        pills.appendChild(empty);
      } else {
        selected.forEach(function (id) {
          var o = byID[id]; if (!o) return;
          var pill = document.createElement('span');
          pill.className = 'ms-pill';
          if (o.color) pill.style.setProperty('--ms-color', o.color);

          var dot = makeDot(o.color);
          if (dot) pill.appendChild(dot);

          var label = document.createElement('span');
          label.textContent = o.name;
          pill.appendChild(label);

          var x = document.createElement('button'); x.type = 'button';
          x.className = 'ms-remove'; x.setAttribute('aria-label', 'Remove ' + o.name);
          x.textContent = '×';
          x.addEventListener('click', function (e) {
            e.stopPropagation();
            selected = selected.filter(function (sid) { return sid !== id; });
            render();
          });
          pill.appendChild(x);
          pills.appendChild(pill);
        });
      }

      // Hidden inputs (one per selected ID).
      root.querySelectorAll('input[type=hidden]').forEach(function (n) { n.remove(); });
      selected.forEach(function (id) {
        var hidden = document.createElement('input');
        hidden.type = 'hidden';
        hidden.name = name;
        hidden.value = id;
        root.appendChild(hidden);
      });

      // Popup options (unselected only, filtered).
      var q = filter.value.trim().toLowerCase();
      list.innerHTML = '';
      var available = options.filter(function (o) {
        if (selected.indexOf(o.id) !== -1) return false;
        return !q || o.name.toLowerCase().indexOf(q) !== -1;
      });
      if (available.length === 0) {
        var none = document.createElement('li');
        none.className = 'ms-none';
        none.textContent = q ? 'No matches' : 'All selected';
        list.appendChild(none);
      } else {
        available.forEach(function (o) {
          var li = document.createElement('li');
          li.className = 'ms-option';
          li.tabIndex = 0;
          var dot = makeDot(o.color);
          if (dot) li.appendChild(dot);
          var lbl = document.createElement('span'); lbl.textContent = o.name;
          li.appendChild(lbl);
          li.addEventListener('click', function () {
            // Single-select mode: replace the existing selection rather than append.
            if (max === 1) {
              selected = [o.id];
              root.classList.remove('open');
            } else if (max > 0 && selected.length >= max) {
              return; // limit reached for multi-select with a cap > 1
            } else {
              selected.push(o.id);
            }
            filter.value = '';
            render();
            // Keep focus in the popup for unbounded multi-select so admins can
            // add several in a row. In single-select mode we closed the popup
            // above, so focus the add button instead.
            if (max === 1) addBtn.focus(); else filter.focus();
          });
          li.addEventListener('keydown', function (e) {
            if (e.key === 'Enter' || e.key === ' ') {
              e.preventDefault();
              li.click();
            }
          });
          list.appendChild(li);
        });
      }
    }

    addBtn.addEventListener('click', function (e) {
      e.stopPropagation();
      var willOpen = !root.classList.contains('open');
      // Close any other open multiselect.
      document.querySelectorAll('.multiselect.open').forEach(function (el) {
        if (el !== root) el.classList.remove('open');
      });
      root.classList.toggle('open', willOpen);
      if (willOpen) { filter.value = ''; render(); filter.focus(); }
    });

    filter.addEventListener('input', render);
    filter.addEventListener('keydown', function (e) {
      if (e.key === 'Escape') {
        root.classList.remove('open');
        addBtn.focus();
      }
    });

    if (options.length === 0) {
      addBtn.disabled = true;
      addBtn.title = 'Nothing available - add Prishe to this server first.';
    }

    render();
  });
})();
