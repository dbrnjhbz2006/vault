/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import Actions from './replication-actions-single';
import layout from '../templates/components/replication-action-generate-token';

export default Actions.extend({
  layout,
  tagName: '',
});
