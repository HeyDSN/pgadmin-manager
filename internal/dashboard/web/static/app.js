const confirmDialog = document.querySelector("#confirm-dialog");
const confirmTitle = confirmDialog?.querySelector("#confirm-title");
const confirmMessage = confirmDialog?.querySelector("#confirm-message");
const confirmSubmit = confirmDialog?.querySelector("[data-confirm-submit]");
const confirmCancel = confirmDialog?.querySelector("[data-confirm-cancel]");
let pendingForm = null;

document.addEventListener("submit", function (event) {
  const form = event.target.closest("form.js-confirm");
  if (!form || form.dataset.confirmed === "true") return;

  event.preventDefault();
  pendingForm = form;
  confirmTitle.textContent = form.dataset.confirmTitle || "Confirm action?";
  confirmMessage.textContent = form.dataset.confirm || "This action cannot be undone.";
  confirmDialog.showModal();
  confirmCancel.focus();
});

confirmCancel?.addEventListener("click", function () {
  pendingForm = null;
  confirmDialog.close();
});

confirmSubmit?.addEventListener("click", function () {
  if (!pendingForm) return;
  const form = pendingForm;
  pendingForm = null;
  form.dataset.confirmed = "true";
  const confirmation = form.querySelector('input[name="confirmed"]');
  if (confirmation) confirmation.value = "yes";
  confirmDialog.close();
  form.requestSubmit();
});

confirmDialog?.addEventListener("cancel", function () {
  pendingForm = null;
});

document.querySelectorAll("dialog[data-auto-open]").forEach(function (dialog) {
  dialog.showModal();
});

document.querySelectorAll("[data-open-dialog]").forEach(function (button) {
  button.addEventListener("click", function () {
    document.getElementById(button.dataset.openDialog)?.showModal();
  });
});

document.querySelectorAll("[data-close-dialog]").forEach(function (button) {
  button.addEventListener("click", function () {
    button.closest("dialog")?.close();
  });
});

document.querySelectorAll("[data-table-search]").forEach(function (input) {
  const table = document.getElementById(input.dataset.tableSearch);
  const body = table?.tBodies[0];
  if (!body) return;

  const rows = Array.from(body.rows).filter(function (row) {
    return !row.querySelector(".empty");
  });
  const noResults = document.createElement("tr");
  noResults.className = "search-empty";
  noResults.hidden = true;
  noResults.innerHTML = `<td colspan="${table.tHead.rows[0].cells.length}" class="empty">No matching results</td>`;
  body.appendChild(noResults);

  input.addEventListener("input", function () {
    const query = input.value.trim().toLocaleLowerCase();
    let visible = 0;
    rows.forEach(function (row) {
      const matches = !query || row.textContent.toLocaleLowerCase().includes(query);
      row.hidden = !matches;
      if (matches) visible++;
    });
    noResults.hidden = visible !== 0;
  });
});

// Delegated so buttons inside modals injected after load work too.
document.addEventListener("click", function (event) {
  const generate = event.target.closest("[data-generate]");
  if (generate) {
    const field = document.getElementById(generate.dataset.generate);
    const chars = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789";
    field.value = Array.from(crypto.getRandomValues(new Uint32Array(20)), function (n) { return chars[n % chars.length]; }).join("");
    field.type = "text";
    return;
  }

  const copy = event.target.closest("[data-copy]");
  const field = copy && document.getElementById(copy.dataset.copy);
  if (!field) return;
  const type = field.type;
  field.type = "text";
  field.select();
  const done = function () {
    copy.textContent = "Copied";
    setTimeout(function () { copy.textContent = "Copy"; }, 1500);
  };
  // clipboard API needs a secure context; plain-HTTP deployments use execCommand
  if (navigator.clipboard && window.isSecureContext) {
    navigator.clipboard.writeText(field.value).then(done);
  } else if (document.execCommand("copy")) {
    done();
  }
  field.type = type;
});

const modalLink = ['add', 'manage', 'password', 'inspect', 'delete'].map(function (key) { return `a[href*="?${key}="]`; }).join(",");

// Open server-rendered modals in place; any failure falls back to normal navigation.
document.addEventListener("click", async function (event) {
  const link = event.target.closest("a[href]");
  if (!link || event.defaultPrevented || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey) return;

  const injected = link.closest("dialog[data-injected]");
  if (injected && !link.search) {
    event.preventDefault();
    injected.close();
    return;
  }
  if (!link.matches(modalLink)) return;

  event.preventDefault();
  try {
    const response = await fetch(link.href);
    const doc = new DOMParser().parseFromString(await response.text(), "text/html");
    const source = doc.querySelector("dialog[data-auto-open]");
    if (!source) throw new Error("no dialog in response");
    const dialog = document.importNode(source, true);
    dialog.removeAttribute("data-auto-open");
    dialog.dataset.injected = "";
    dialog.addEventListener("close", function () { dialog.remove(); });
    document.body.append(dialog);
    dialog.showModal();
  } catch (error) {
    window.location.href = link.href;
  }
});

document.querySelectorAll("#users-table tbody td:first-child strong, .dashboard-user-row strong").forEach(addAvatar(true));
document.querySelectorAll("#databases-table tbody td:first-child strong, .dashboard-inventory td:first-child strong, .ownership-item strong").forEach(addAvatar(false));

function addAvatar(round) {
  return function (name) {
    const text = name.textContent.trim();
    if (!text) return;
    let hash = 0;
    for (const char of text) hash += char.charCodeAt(0);
    const avatar = document.createElement("span");
    avatar.className = `avatar hue-${hash % 8}${round ? " round" : ""}`;
    avatar.setAttribute("aria-hidden", "true");
    avatar.textContent = text[0];
    name.classList.add("has-avatar");
    name.prepend(avatar);
  };
}

document.addEventListener("keydown", function (event) {
  if (event.key !== "/" || /^(INPUT|TEXTAREA|SELECT)$/.test(document.activeElement.tagName)) return;
  const search = document.querySelector("[data-table-search]");
  if (!search) return;
  event.preventDefault();
  search.focus();
});

// Labels let CSS turn table rows into cards on narrow screens.
document.querySelectorAll("table").forEach(function (table) {
  const heads = Array.from(table.tHead?.rows[0]?.cells ?? [], function (cell) { return cell.textContent.trim(); });
  Array.from(table.tBodies[0]?.rows ?? []).forEach(function (row) {
    Array.from(row.cells).forEach(function (cell, index) {
      if (cell.colSpan === 1) cell.dataset.label = heads[index] ?? "";
    });
  });
});
