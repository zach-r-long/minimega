'use strict';

(function() {
  const template = ''
    + '<tr'
    + '  :class="{active: selected}"'
    + '  class="res clickable"'
    + '  v-on:click.stop="selectReservation(reservation)"'
    + '>'
    + '  <td>{{ reservation.Name }}</td>'
    + '  <td>{{ reservation.Owner }}</td>'
    + '  <td>{{ reservation.Group }}</td>'
    + '  <td class="current">{{ reservation.Start }}</td>'
    + '  <td>{{ reservation.End }}</td>'
    + '  <td>{{ nodeCount }}</td>'
    + '  <td>{{ reservation.Range }}</td>'
    + '  <td v-if="reservation.CanEdit">'
    + '    <button'
    + '      class="btn btn-primary"'
    + '      v-on:click="$emit(\'res-action\', \'edit\', reservation.Name)"'
    + '    >'
    + '      <i class="oi oi-pencil"></i>'
    + '    </button>'
    + '  </td>'
    + '</tr>'
    + '';
  window.ReservationTableRow = {
    template: template,
    props: {
      reservation: {
        type: Object,
      },
    },
    computed: {
      nodeCount: function nodeCount() {
        return this.reservation.Nodes.length;
      },
      selected: function selected() {
        if (this.$store.state.selectedReservation == null) {
          return false;
        }

        return this.$store.state.selectedReservation.Name == this.reservation.Name;
      },
    },
    methods: {
      selectReservation: function selectReservation(r) {
        this.$store.dispatch('selectReservation', r);
      },
    },
  };
})();
