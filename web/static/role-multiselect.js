// Chip-style multi-select for the per-command role-lock UI.
//
// Each <div class="role-multiselect" data-name="roles_<cmd>" data-selected="id1,id2">
// becomes a self-rendering control that shows selected roles as colored pills
// and exposes an "Add role" popover with type-to-filter for picking more.
// Hidden inputs named data-name carry the values to the server on submit.
//
// Roles come from window.PRISHE_ROLES — [{id, name, color}, ...] — set inline
// by the server template.
(function () {
  var roles = window.PRISHE_ROLES || [];
  var byID = {};
  roles.forEach(function (r) { byID[r.id] = r; });

  // Close any open popover when clicking outside any multiselect.
  document.addEventListener('click', function (e) {
    document.querySelectorAll('.role-multiselect.open').forEach(function (el) {
      if (!el.contains(e.target)) el.classList.remove('open');
    });
  });

  document.querySelectorAll('.role-multiselect').forEach(function (root) {
    var name = root.dataset.name;
    var initial = (root.dataset.selected || '').split(',').filter(Boolean);
    var selected = initial.filter(function (id) { return byID[id]; });

    // Markup scaffold.
    var pills    = document.createElement('div'); pills.className    = 'rms-pills';
    var control  = document.createElement('div'); control.className  = 'rms-control';
    var addBtn   = document.createElement('button'); addBtn.type     = 'button';
    addBtn.className = 'rms-add'; addBtn.textContent = '+ Add role';
    var popup    = document.createElement('div'); popup.className    = 'rms-popup';
    var filter   = document.createElement('input');
    filter.type  = 'search'; filter.className = 'rms-filter'; filter.placeholder = 'Filter roles…';
    var list     = document.createElement('ul'); list.className = 'rms-options';
    popup.appendChild(filter); popup.appendChild(list);
    control.appendChild(addBtn); control.appendChild(popup);
    root.appendChild(pills); root.appendChild(control);

    function render() {
      // Pills.
      pills.innerHTML = '';
      if (selected.length === 0) {
        var empty = document.createElement('span');
        empty.className = 'rms-empty';
        empty.textContent = 'Admin-only';
        pills.appendChild(empty);
      } else {
        selected.forEach(function (id) {
          var r = byID[id]; if (!r) return;
          var pill = document.createElement('span');
          pill.className = 'rms-pill';
          pill.style.setProperty('--rms-color', r.color);

          var dot = document.createElement('span'); dot.className = 'rms-dot';
          dot.style.background = r.color;
          var label = document.createElement('span'); label.textContent = r.name;
          var x = document.createElement('button'); x.type = 'button';
          x.className = 'rms-remove'; x.setAttribute('aria-label', 'Remove ' + r.name);
          x.textContent = '×';
          x.addEventListener('click', function (e) {
            e.stopPropagation();
            selected = selected.filter(function (sid) { return sid !== id; });
            render();
          });
          pill.appendChild(dot); pill.appendChild(label); pill.appendChild(x);
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
      var available = roles.filter(function (r) {
        if (selected.indexOf(r.id) !== -1) return false;
        return !q || r.name.toLowerCase().indexOf(q) !== -1;
      });
      if (available.length === 0) {
        var none = document.createElement('li');
        none.className = 'rms-none';
        none.textContent = q ? 'No matches' : 'All roles selected';
        list.appendChild(none);
      } else {
        available.forEach(function (r) {
          var li = document.createElement('li');
          li.className = 'rms-option';
          li.tabIndex = 0;
          var dot = document.createElement('span'); dot.className = 'rms-dot';
          dot.style.background = r.color;
          var lbl = document.createElement('span'); lbl.textContent = r.name;
          li.appendChild(dot); li.appendChild(lbl);
          li.addEventListener('click', function () {
            selected.push(r.id);
            filter.value = '';
            render();
            // Keep focus in the popup so admins can add several roles in a row.
            filter.focus();
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
      document.querySelectorAll('.role-multiselect.open').forEach(function (el) {
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

    if (roles.length === 0) {
      addBtn.disabled = true;
      addBtn.title = 'No roles available — add Prishe to this server first.';
    }

    render();
  });
})();
