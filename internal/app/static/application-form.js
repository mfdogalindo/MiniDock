(() => {
  const form = document.querySelector('#application-wizard');
  if (!form) return;
  const pages = [...form.querySelectorAll('.wizard-page')];
  const steps = [...document.querySelectorAll('.step')];
  const browser = document.querySelector('#repository-browser');
  const repository = document.querySelector('#repository');
  const branch = form.elements.branch;
  const runtimeType = form.elements.type;
  const detectedRuntime = document.createElement('p');
  detectedRuntime.className = 'hint';
  runtimeType.parentElement.after(detectedRuntime);
  const runtimeOptions = {
    angular_static: 'Angular estático',
    node_ssr: 'Node SSR (start)',
	    vite_ssr: 'Vite SSR (servidor Node)',
    astro_static: 'Astro estático',
    astro_ssr: 'Astro SSR (Node)',
    nuxt_static: 'Nuxt estático',
    nuxt_ssr: 'Nuxt SSR (Nitro)',
    svelte_static: 'SvelteKit estático',
    svelte_ssr: 'SvelteKit SSR (Node)'
  };
  Object.entries(runtimeOptions).forEach(([value, label]) => {
    if (![...runtimeType.options].some(option => option.value === value)) {
      const option = document.createElement('option'); option.value = value; option.textContent = label; runtimeType.append(option);
    }
  });
  const selectorStyle = document.createElement('style');
  selectorStyle.textContent = `.custom-select{position:relative;margin-top:7px}.custom-select-trigger{width:100%;justify-content:space-between;background:#080808;color:var(--paper);border:1px solid var(--line);text-align:left}.custom-select-trigger::after{content:'↓';color:var(--acid)}.custom-select-trigger[aria-expanded="true"]{border-color:var(--acid)}.custom-select-options{position:absolute;z-index:10;left:0;right:0;max-height:16rem;overflow:auto;border:1px solid var(--acid);background:var(--panel);box-shadow:5px 5px 0 #000}.custom-select-options[hidden]{display:none}.custom-select-option{display:block;width:100%;min-height:38px;border:0;border-bottom:1px solid var(--line);background:transparent;color:var(--paper);padding:.55rem .75rem;text-align:left;font:inherit;cursor:pointer}.custom-select-option:hover,.custom-select-option[aria-selected="true"]{background:var(--acid);color:var(--ink)}`;
  document.head.append(selectorStyle);
  const enhancedSelects = new Map();
  const enhanceSelect = select => {
    select.hidden = true;
    select.style.display = 'none';
    const root = document.createElement('div'); root.className = 'custom-select';
    const trigger = document.createElement('button'); trigger.type = 'button'; trigger.className = 'custom-select-trigger'; trigger.setAttribute('aria-haspopup', 'listbox');
    const options = document.createElement('div'); options.className = 'custom-select-options'; options.setAttribute('role', 'listbox'); options.hidden = true;
    const close = () => { options.hidden = true; trigger.setAttribute('aria-expanded', 'false'); };
    const refresh = () => {
      trigger.textContent = select.options[select.selectedIndex]?.text || 'Seleccionar';
      options.replaceChildren();
      [...select.options].forEach((option, index) => {
        const choice = document.createElement('button'); choice.type = 'button'; choice.className = 'custom-select-option'; choice.textContent = option.text; choice.setAttribute('role', 'option'); choice.setAttribute('aria-selected', String(index === select.selectedIndex));
        choice.onclick = () => { select.selectedIndex = index; select.dispatchEvent(new Event('change', {bubbles: true})); refresh(); close(); trigger.focus(); };
        options.append(choice);
      });
    };
    trigger.onclick = () => { const open = options.hidden; document.querySelectorAll('.custom-select-options').forEach(list => { list.hidden = true; }); options.hidden = !open; trigger.setAttribute('aria-expanded', String(open)); };
    trigger.onkeydown = event => { if (event.key === 'ArrowDown' || event.key === 'Enter' || event.key === ' ') { event.preventDefault(); options.hidden = false; trigger.setAttribute('aria-expanded', 'true'); options.querySelector('[aria-selected="true"]')?.focus(); } };
    options.onkeydown = event => { const choices = [...options.querySelectorAll('button')]; const index = choices.indexOf(document.activeElement); if (event.key === 'Escape') { close(); trigger.focus(); } if (event.key === 'ArrowDown' && index < choices.length - 1) { event.preventDefault(); choices[index + 1].focus(); } if (event.key === 'ArrowUp' && index > 0) { event.preventDefault(); choices[index - 1].focus(); } };
    root.append(trigger, options); select.after(root); refresh(); enhancedSelects.set(select, {refresh});
  };
  enhanceSelect(runtimeType);
  enhanceSelect(form.elements.runtime);
  document.addEventListener('click', event => { if (!event.target.closest('.custom-select')) document.querySelectorAll('.custom-select-options').forEach(options => { options.hidden = true; }); });
  const references = document.createElement('datalist');
  references.id = 'git-references';
  document.body.append(references);
  branch.setAttribute('list', references.id);
  let current = 0;
  const show = index => {
    current = index;
    pages.forEach((page, i) => { page.hidden = i !== index; });
    steps.forEach((step, i) => step.classList.toggle('active', i === index));
    if (index === 2) form.querySelectorAll('[data-review]').forEach(item => {
      const input = form.elements[item.dataset.review];
      item.textContent = input.options ? input.options[input.selectedIndex].text : input.value || '—';
    });
  };
  const detectRuntime = async path => {
    detectedRuntime.textContent = 'Analizando manifiestos del proyecto…';
    try {
      const response = await fetch('/repositories/detect?path=' + encodeURIComponent(path));
      if (!response.ok) throw new Error();
      const detection = await response.json();
      if ([...runtimeType.options].some(option => option.value === detection.type)) { runtimeType.value = detection.type; enhancedSelects.get(runtimeType).refresh(); }
      detectedRuntime.textContent = `Sugerencia (${detection.confidence}): ${detection.reason}`;
    } catch (_) { detectedRuntime.textContent = 'No se pudo detectar un runtime; selecciona uno manualmente.'; }
  };
  const browse = async path => {
    browser.hidden = false;
    browser.textContent = 'Cargando…';
    try {
      const response = await fetch('/repositories/browse?path=' + encodeURIComponent(path || ''));
      if (!response.ok) throw new Error(response.status === 503 ? 'El explorador local no está configurado.' : 'No se puede abrir esta carpeta.');
      const data = await response.json();
      browser.replaceChildren();
      const head = document.createElement('div');
      head.className = 'browser-head';
      const location = document.createElement('span');
      location.className = 'browser-path';
      location.textContent = data.current;
      head.append(location);
      if (data.parent !== undefined) {
        const up = document.createElement('button');
        up.type = 'button'; up.textContent = '↑'; up.onclick = () => browse(data.parent);
        head.append(up);
      }
      const create = document.createElement('button');
      create.type = 'button'; create.textContent = '+ Carpeta';
      create.onclick = async () => {
        const name = window.prompt('Nombre de la nueva carpeta');
        if (!name) return;
        const result = await fetch('/repositories/folders', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({path: path || '', name})});
        if (!result.ok) { browser.textContent = 'No se pudo crear la carpeta.'; return; }
        await browse(path);
      };
      head.append(create);
      browser.append(head);
      if (!data.entries.length) {
        const empty = document.createElement('p'); empty.className = 'browser-empty'; empty.textContent = 'No hay carpetas disponibles.'; browser.append(empty);
      }
      data.entries.forEach(entry => {
        const row = document.createElement('div'); row.className = 'browser-entry';
        const name = document.createElement('span'); name.textContent = (entry.repository ? '◈ ' : '▸ ') + entry.name; row.append(name);
        const actions = document.createElement('span');
        const open = document.createElement('button'); open.type = 'button'; open.textContent = 'Abrir'; open.onclick = () => browse(entry.path); actions.append(open);
        const select = document.createElement('button'); select.type = 'button'; select.textContent = entry.repository ? 'Usar Git' : 'Usar código';
        select.onclick = async () => {
          repository.value = data.current.replace(/^file:\/\//, 'file://') + '/' + entry.name;
          await detectRuntime(entry.path);
          if (!entry.repository) {
            branch.value = ''; branch.required = false; branch.placeholder = 'No se necesita para una carpeta sin Git'; references.replaceChildren(); browser.hidden = true; return;
          }
          branch.required = true; branch.placeholder = 'Selecciona o escribe una rama, tag o ref'; references.replaceChildren();
          try {
            const response = await fetch('/repositories/refs?path=' + encodeURIComponent(entry.path));
            if (!response.ok) throw new Error();
            const data = await response.json();
            data.references.forEach(reference => { const option = document.createElement('option'); option.value = reference; references.append(option); });
            if (!branch.value && data.references.length) branch.value = data.references[0];
          } catch (_) { branch.placeholder = 'Escribe la rama, tag o ref'; }
          browser.hidden = true;
        };
        actions.append(select);
        row.append(actions); browser.append(row);
      });
    } catch (error) { browser.textContent = error.message; }
  };
  document.querySelector('#browse-repositories').onclick = () => browse('');
  form.addEventListener('click', event => {
    if (event.target.classList.contains('next')) {
      const inputs = [...pages[current].querySelectorAll('input,select')];
      if (inputs.every(input => input.reportValidity())) show(current + 1);
    }
    if (event.target.classList.contains('previous')) show(current - 1);
  });
  show(0);
})();
